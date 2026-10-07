package media

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	_ "image/png"
	"io"
	"math"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

type ObjectInfo struct {
	Size        int64
	ContentType string
}

// PresentationMetadata is stable information extracted from the original
// asset. It lets presentation clients preserve aspect ratio and choose a safe
// video transition cue without decoding or analysing media in their render
// loop.
type PresentationMetadata struct {
	Width                *int
	Height               *int
	DurationSeconds      *float64
	TransitionCueSeconds *float64
}

type Storage interface {
	PresignPut(ctx context.Context, key, mime string, expiry time.Duration) (string, error)
	PresignGet(ctx context.Context, key string, expiry time.Duration) (string, error)
	Head(ctx context.Context, key string) (ObjectInfo, error)
	Delete(ctx context.Context, key string) error
	// DeleteMany removes several objects and returns the keys it could not
	// remove. An empty result means every key is gone. Providers bill and rate
	// limit per request, so cleanup batches one call instead of one per object.
	DeleteMany(ctx context.Context, keys []string) ([]string, error)
	OpenRead(ctx context.Context, key string) (io.ReadCloser, ObjectInfo, error)
	Put(ctx context.Context, key, mime string, body io.Reader) error
}

type Limiter interface {
	Allow(context.Context, string, int, time.Duration) (bool, error)
}

type Service struct {
	repo               Repository
	storage            Storage
	limiter            Limiter
	expiry, window     time.Duration
	rate               int
	maxImage, maxVideo int64
	notifier           Notifier
}

// NewService takes an optional Notifier. Realtime broadcast is a deployment
// concern, not a precondition for accepting uploads: with no notifier the
// service behaves exactly as before.
func NewService(repo Repository, storage Storage, limiter Limiter, expiry, window time.Duration, rate int, maxImage, maxVideo int64, notifiers ...Notifier) *Service {
	s := &Service{repo: repo, storage: storage, limiter: limiter, expiry: expiry, window: window, rate: rate, maxImage: maxImage, maxVideo: maxVideo}
	if len(notifiers) > 0 {
		s.notifier = notifiers[0]
	}
	return s
}

// notify is always called after the change is durable, so a browser can never
// be told about state the database would not confirm on the next read.
func (s *Service) notify(ctx context.Context, change Change) {
	if s.notifier == nil {
		return
	}
	s.notifier.MediaChanged(ctx, change)
}

type CreateInput struct {
	Filename, MIMEType string
	Size               int64
	ChecksumSHA256     string
	SessionToken       string
	ClientUploadID     uuid.UUID
	GuestName          string
	Caption            string
}
type UploadTarget struct {
	MediaID         uuid.UUID         `json:"media_id"`
	UploadURL       string            `json:"upload_url"`
	ExpiresAt       time.Time         `json:"expires_at"`
	RequiredHeaders map[string]string `json:"required_headers"`
}

type UploadScope struct {
	EventID        uuid.UUID
	GuestSessionID uuid.UUID
	Accepting      bool
	MaxEventBytes  int64
	// EnforceItems applies the plan's item limits to new reservations.
	EnforceItems bool
	// UploadClosed rejects new reservations because the event's upload window
	// has ended. Uploads already reserved may still complete.
	UploadClosed bool
}

func (s *Service) CreateUpload(ctx context.Context, scope UploadScope, in CreateInput) (UploadTarget, error) {
	if !scope.Accepting {
		return UploadTarget{}, fmt.Errorf("event is not accepting uploads")
	}
	if in.Size <= 0 || !allowed(in.MIMEType) || in.Size > limitByMIME(in.MIMEType, s.maxImage, s.maxVideo) || !validSHA256(in.ChecksumSHA256) {
		return UploadTarget{}, fmt.Errorf("invalid media type or file size")
	}
	if in.ClientUploadID == uuid.Nil {
		return UploadTarget{}, fmt.Errorf("client upload id is required")
	}
	// A browser can lose the response after the reservation committed. Reuse the
	// same record/object key on the next request rather than reserving quota again.
	m, existingErr := s.repo.FindByClientUpload(ctx, scope.EventID, scope.GuestSessionID, in.ClientUploadID)
	created := false
	if existingErr != nil && !errors.Is(existingErr, ErrNotFound) {
		return UploadTarget{}, existingErr
	}
	if errors.Is(existingErr, ErrNotFound) {
		if scope.UploadClosed {
			return UploadTarget{}, ErrUploadClosed
		}
		duplicate, err := s.repo.HasReadyChecksum(ctx, scope.EventID, in.ChecksumSHA256)
		if err != nil {
			return UploadTarget{}, err
		}
		if duplicate {
			return UploadTarget{}, ErrDuplicate
		}
		ok, err := s.limiter.Allow(ctx, "upload:"+scope.GuestSessionID.String(), s.rate, s.window)
		if err != nil {
			return UploadTarget{}, err
		}
		if !ok {
			return UploadTarget{}, fmt.Errorf("upload rate limit exceeded")
		}
		clientID := in.ClientUploadID
		m = Media{ID: uuid.New(), EventID: scope.EventID, GuestSessionID: scope.GuestSessionID, OriginalFilename: filepath.Base(in.Filename), MIMEType: in.MIMEType, GuestName: optionalText(in.GuestName), Caption: optionalText(in.Caption), ExpectedSize: in.Size, ChecksumSHA256: strings.ToLower(in.ChecksumSHA256), ClientUploadID: &clientID, Status: StatusPending, LastActivityAt: time.Now()}
		m.ObjectKey = fmt.Sprintf("events/%s/media/%s/original%s", scope.EventID, m.ID, extension(in.MIMEType))
		err = s.repo.ReserveUpload(ctx, m, ReserveLimits{MaxEventBytes: scope.MaxEventBytes, EnforceItems: scope.EnforceItems})
		if err != nil {
			// A concurrent duplicate request may have won the unique client ID race.
			if existing, findErr := s.repo.FindByClientUpload(ctx, scope.EventID, scope.GuestSessionID, in.ClientUploadID); findErr == nil {
				m = existing
			} else {
				return UploadTarget{}, err
			}
		} else {
			created = true
		}
	}
	url, err := s.storage.PresignPut(ctx, m.ObjectKey, m.MIMEType, s.expiry)
	if err != nil {
		if created {
			_ = s.repo.Delete(ctx, m.ID)
		}
		return UploadTarget{}, err
	}
	return UploadTarget{
		MediaID:   m.ID,
		UploadURL: url,
		ExpiresAt: time.Now().Add(s.expiry),
		RequiredHeaders: map[string]string{
			"Content-Type":  m.MIMEType,
			"Cache-Control": "private, max-age=1800",
		},
	}, nil
}

func optionalText(value string) *string {
	trimmed := strings.TrimSpace(value)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func (s *Service) Complete(ctx context.Context, scope UploadScope, mediaID uuid.UUID) error {
	if !scope.Accepting {
		return fmt.Errorf("event is not accepting uploads")
	}
	m, err := s.repo.FindUploadForSession(ctx, mediaID, scope.EventID, scope.GuestSessionID)
	if err != nil {
		return err
	}
	if m.Status == StatusReady {
		return nil
	}
	obj, err := s.storage.Head(ctx, m.ObjectKey)
	if err != nil {
		return fmt.Errorf("uploaded object is unavailable: %w", err)
	}
	if obj.Size != m.ExpectedSize || obj.ContentType != m.MIMEType {
		return fmt.Errorf("uploaded object metadata does not match requested upload")
	}
	err = s.repo.MarkReady(ctx, scope.EventID, m.ID, obj.Size, time.Now())
	if errors.Is(err, ErrNotFound) {
		// Another complete request may have won the race. Treat it as success if
		// it made the same event/session-owned record ready.
		if current, findErr := s.repo.FindUploadForSession(ctx, mediaID, scope.EventID, scope.GuestSessionID); findErr == nil && current.Status == StatusReady {
			return nil
		}
	}
	if errors.Is(err, ErrDuplicate) {
		_ = s.storage.Delete(ctx, m.ObjectKey)
		_ = s.repo.Delete(ctx, m.ID)
	}
	if err != nil {
		return err
	}
	m.Status = StatusReady
	item := newItem(m)
	s.notify(ctx, Change{Kind: ChangeCreated, EventID: scope.EventID, IDs: []uuid.UUID{m.ID}, Status: StatusReady, Item: &item})
	// The thumbnail is not rendered here. MarkReady enqueued it in the same
	// transaction that made this record durable, so a worker owns it from now
	// on: it survives a restart, it retries, and no upload burst can start
	// more image decodes than the worker pool allows.
	return nil
}

// MaxCompleteBatch bounds one batch confirmation. It is generous enough for a
// guest emptying a camera roll and small enough that one request cannot hold a
// large share of the database pool.
const MaxCompleteBatch = 50

// completeConcurrency is how many confirmations run at once. Each one holds a
// database connection while it verifies the object with the storage provider,
// so this trades latency against the pool the rest of the event shares.
const completeConcurrency = 6

// CompleteResult reports one media item's outcome. A batch is not all or
// nothing: a guest with thirty good photos and one that failed to upload must
// keep the thirty and retry only the one.
type CompleteResult struct {
	MediaID uuid.UUID `json:"media_id"`
	Status  string    `json:"status"`
	Error   string    `json:"error,omitempty"`
}

// CompleteMany confirms several uploads in one call.
//
// Confirming is a network round trip to storage to verify the object, plus a
// transaction. Done one request at a time, a guest sharing forty photos on
// venue wifi pays forty sequential round trips; here they overlap.
//
// It returns one result per requested id, in the order given.
func (s *Service) CompleteMany(ctx context.Context, scope UploadScope, ids []uuid.UUID) ([]CompleteResult, error) {
	ids = uniqueMediaIDs(ids)
	if len(ids) == 0 || len(ids) > MaxCompleteBatch {
		return nil, fmt.Errorf("between 1 and %d media ids are required", MaxCompleteBatch)
	}
	if !scope.Accepting {
		return nil, fmt.Errorf("event is not accepting uploads")
	}

	results := make([]CompleteResult, len(ids))
	slots := make(chan struct{}, completeConcurrency)
	var wg sync.WaitGroup
	for i, id := range ids {
		wg.Add(1)
		go func(i int, id uuid.UUID) {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()

			results[i] = CompleteResult{MediaID: id, Status: "ready"}
			if err := s.Complete(ctx, scope, id); err != nil {
				results[i].Status = "failed"
				results[i].Error = err.Error()
			}
		}(i, id)
	}
	wg.Wait()
	return results, nil
}

func thumbnailKey(m Media) string {
	return fmt.Sprintf("events/%s/media/%s/thumbnail.jpg", m.EventID, m.ID)
}

// maxThumbnailPixels bounds what one job may decode. A decoded image costs
// roughly four bytes per pixel before the resized copy is allocated, so this
// is the difference between a worker slot and an out-of-memory kill. Anything
// larger keeps the original as its gallery fallback.
const maxThumbnailPixels = 50_000_000

// GenerateThumbnail renders the gallery-sized variant of one image or video. It is the
// unit of work behind a mediajob, so its result decides whether the job is
// retried: a returned error means "try again later", and nil means the record
// needs nothing further.
//
// Nothing about it is on the guest's confirmation path any more.
func (s *Service) GenerateThumbnail(ctx context.Context, mediaID uuid.UUID) error {
	m, err := s.repo.FindByID(ctx, mediaID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			// Deleted between enqueue and claim. Nothing is owed.
			return nil
		}
		return err
	}
	if m.ThumbnailReady && hasPresentationMetadata(m) {
		return nil
	}

	if strings.HasPrefix(m.MIMEType, "image/") {
		return s.generateImageThumbnail(ctx, m)
	}
	if strings.HasPrefix(m.MIMEType, "video/") {
		return s.generateVideoThumbnail(ctx, m)
	}

	return nil
}

func (s *Service) generateImageThumbnail(ctx context.Context, m Media) error {
	body, _, err := s.storage.OpenRead(ctx, m.ObjectKey)
	if err != nil {
		// The object may still be replicating, or storage may be briefly
		// unavailable. Both are worth another attempt.
		return fmt.Errorf("read original: %w", err)
	}
	defer body.Close()

	// The header is decoded first so an image too large to hold in memory is
	// rejected before it is allocated. TeeReader keeps the consumed bytes so
	// the full decode can still see a complete stream.
	var header bytes.Buffer
	config, _, err := image.DecodeConfig(io.TeeReader(body, &header))
	if err != nil {
		// Formats Go cannot decode (HEIC, for one) are permanent. Retrying
		// would burn five attempts to reach the same answer.
		return nil
	}
	metadata := PresentationMetadata{Width: intPointer(config.Width), Height: intPointer(config.Height)}
	if config.Width < 1 || config.Height < 1 {
		return nil
	}
	if m.ThumbnailReady || config.Width*config.Height > maxThumbnailPixels {
		return s.updateProcessingResult(ctx, m, m.ThumbnailReady, metadata)
	}

	source, _, err := image.Decode(io.MultiReader(bytes.NewReader(header.Bytes()), body))
	if err != nil {
		return nil
	}
	bounds := source.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if width < 1 || height < 1 {
		return nil
	}
	const maxDimension = 720
	if width > height && width > maxDimension {
		height = height * maxDimension / width
		width = maxDimension
	} else if height > maxDimension {
		width = width * maxDimension / height
		height = maxDimension
	}
	thumbnail := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.CatmullRom.Scale(thumbnail, thumbnail.Bounds(), source, bounds, draw.Over, nil)
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, thumbnail, &jpeg.Options{Quality: 82}); err != nil {
		return nil
	}
	if err := s.storage.Put(ctx, thumbnailKey(m), "image/jpeg", &encoded); err != nil {
		return fmt.Errorf("store thumbnail: %w", err)
	}
	if err := s.updateProcessingResult(ctx, m, true, metadata); err != nil {
		return err
	}
	s.notify(ctx, Change{Kind: ChangeThumbnail, EventID: m.EventID, IDs: []uuid.UUID{m.ID}})
	return nil
}

func (s *Service) generateVideoThumbnail(ctx context.Context, m Media) error {
	metadata := s.probeVideoMetadata(ctx, m)
	if m.ThumbnailReady {
		return s.updateProcessingResult(ctx, m, true, metadata)
	}

	ffmpegPath, err := exec.LookPath("ffmpeg")
	if err != nil {
		// ffmpeg is not available on this host or container environment.
		// Persist any metadata ffprobe could still read, then skip extraction
		// gracefully without failing the job.
		return s.updateProcessingResult(ctx, m, false, metadata)
	}

	// Presign a GET URL for the video so FFmpeg can fetch keyframes directly
	// via HTTP range requests without downloading the entire video to server disk/memory.
	downloadURL, err := s.storage.PresignGet(ctx, m.ObjectKey, 5*time.Minute)
	if err != nil {
		return fmt.Errorf("presign video for thumbnail: %w", err)
	}

	// Bound extraction time to 25 seconds to protect worker concurrency slots.
	ffmpegCtx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()

	var stdout, stderr bytes.Buffer
	// -ss 00:00:01.000: seek to 1 second (near first keyframe)
	// -noaccurate_seek: fast seek using keyframes without decoding from 0:00
	// -i: input presigned URL
	// -vframes 1: extract single frame
	// -vf "scale='min(720,iw)':-2": scale to max 720 width while preserving aspect ratio (divisible by 2)
	// -q:v 2: high quality JPEG
	// -f image2 -: output JPEG to stdout pipe
	cmd := exec.CommandContext(ffmpegCtx, ffmpegPath,
		"-ss", "00:00:01.000",
		"-noaccurate_seek",
		"-i", downloadURL,
		"-vframes", "1",
		"-vf", "scale='min(720,iw)':-2",
		"-q:v", "2",
		"-f", "image2",
		"-",
	)
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil || stdout.Len() == 0 {
		// If video is shorter than 1s or seek failed, retry at 00:00:00.000:
		stdout.Reset()
		stderr.Reset()
		cmdRetry := exec.CommandContext(ffmpegCtx, ffmpegPath,
			"-ss", "00:00:00.000",
			"-i", downloadURL,
			"-vframes", "1",
			"-vf", "scale='min(720,iw)':-2",
			"-q:v", "2",
			"-f", "image2",
			"-",
		)
		cmdRetry.Stdout = &stdout
		cmdRetry.Stderr = &stderr
		if errRetry := cmdRetry.Run(); errRetry != nil || stdout.Len() == 0 {
			// Corrupt or unsupported video container format. Retrying won't help,
			// so return nil to retire the job.
			return nil
		}
	}

	if stdout.Len() == 0 {
		return nil
	}

	if err := s.storage.Put(ctx, thumbnailKey(m), "image/jpeg", &stdout); err != nil {
		return fmt.Errorf("store video thumbnail: %w", err)
	}

	if err := s.updateProcessingResult(ctx, m, true, metadata); err != nil {
		return err
	}

	s.notify(ctx, Change{Kind: ChangeThumbnail, EventID: m.EventID, IDs: []uuid.UUID{m.ID}})
	return nil
}

func (s *Service) updateProcessingResult(ctx context.Context, m Media, thumbnailReady bool, metadata PresentationMetadata) error {
	if err := s.repo.UpdateProcessingResult(ctx, m.EventID, m.ID, thumbnailReady, metadata); err != nil {
		if errors.Is(err, ErrNotFound) {
			// Moderated or deleted while this ran. Any generated object is
			// harmless and normal cleanup will remove it with the media.
			return nil
		}
		return fmt.Errorf("persist media processing result: %w", err)
	}
	return nil
}

func hasPresentationMetadata(m Media) bool {
	if m.Width == nil || m.Height == nil {
		return false
	}
	if strings.HasPrefix(m.MIMEType, "video/") {
		return m.DurationSeconds != nil && m.TransitionCueSeconds != nil
	}
	return true
}

type videoProbeOutput struct {
	Streams []struct {
		Width    int    `json:"width"`
		Height   int    `json:"height"`
		Duration string `json:"duration"`
		Tags     struct {
			Rotate string `json:"rotate"`
		} `json:"tags"`
		SideDataList []struct {
			Rotation float64 `json:"rotation"`
		} `json:"side_data_list"`
	} `json:"streams"`
	Format struct {
		Duration string `json:"duration"`
	} `json:"format"`
}

func (s *Service) probeVideoMetadata(ctx context.Context, m Media) PresentationMetadata {
	ffprobePath, err := exec.LookPath("ffprobe")
	if err != nil {
		return PresentationMetadata{}
	}
	downloadURL, err := s.storage.PresignGet(ctx, m.ObjectKey, 5*time.Minute)
	if err != nil {
		return PresentationMetadata{}
	}
	probeCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	output, err := exec.CommandContext(probeCtx, ffprobePath,
		"-v", "error",
		"-select_streams", "v:0",
		"-show_entries", "stream=width,height,duration:stream_tags=rotate:stream_side_data=rotation:format=duration",
		"-of", "json",
		downloadURL,
	).Output()
	if err != nil {
		return PresentationMetadata{}
	}
	return parseVideoProbe(output)
}

func parseVideoProbe(output []byte) PresentationMetadata {
	var probe videoProbeOutput
	if json.Unmarshal(output, &probe) != nil || len(probe.Streams) == 0 {
		return PresentationMetadata{}
	}
	stream := probe.Streams[0]
	width, height := stream.Width, stream.Height
	rotation, _ := strconv.ParseFloat(stream.Tags.Rotate, 64)
	if len(stream.SideDataList) > 0 {
		rotation = stream.SideDataList[0].Rotation
	}
	quarterTurns := int(math.Round(math.Abs(rotation)/90)) % 2
	if quarterTurns == 1 {
		width, height = height, width
	}
	metadata := PresentationMetadata{}
	if width > 0 && height > 0 {
		metadata.Width = intPointer(width)
		metadata.Height = intPointer(height)
	}
	duration, err := strconv.ParseFloat(stream.Duration, 64)
	if err != nil || duration <= 0 {
		duration, _ = strconv.ParseFloat(probe.Format.Duration, 64)
	}
	if duration > 0 && !math.IsNaN(duration) && !math.IsInf(duration, 0) {
		duration = math.Round(duration*1000) / 1000
		metadata.DurationSeconds = floatPointer(duration)
		metadata.TransitionCueSeconds = transitionCueForDuration(duration)
	}
	return metadata
}

func transitionCueForDuration(duration float64) *float64 {
	if duration <= 0 || math.IsNaN(duration) || math.IsInf(duration, 0) {
		return nil
	}
	if duration <= 1 {
		return floatPointer(math.Round(duration*500) / 1000)
	}
	margin := math.Min(1.5, math.Max(0.35, duration*0.12))
	cue := math.Max(margin, math.Min(duration-margin, duration*0.42))
	cue = math.Round(cue*1000) / 1000
	return floatPointer(cue)
}

func intPointer(value int) *int           { return &value }
func floatPointer(value float64) *float64 { return &value }

// ExpireStale removes abandoned reservations. A later retry always creates or
// reuses a client-idempotent upload record, so stale rows must not consume an
// event's upload quota forever.
// One item that cannot be cleaned up must not hold up the rest. FindStale
// orders by last_activity_at, so a record that fails every time would
// otherwise sit at the head of the batch and block every later run, and the
// quota those reservations hold would never come back.
//
// Objects are removed in one provider call and the rows in one statement, so
// a batch of a hundred costs two round trips rather than two hundred.
func (s *Service) ExpireStale(ctx context.Context, before time.Time, limit int) error {
	items, err := s.repo.FindStale(ctx, before, limit)
	if err != nil {
		return err
	}
	if len(items) == 0 {
		return nil
	}

	keys := make([]string, 0, len(items))
	for _, item := range items {
		keys = append(keys, item.ObjectKey)
	}
	unremoved, deleteErr := s.storage.DeleteMany(ctx, keys)

	// Only rows whose object is actually gone are removed. Anything left
	// behind keeps its reservation and is retried by the next run, which is
	// why a partial failure is safe rather than a leak.
	stuck := make(map[string]struct{}, len(unremoved))
	for _, key := range unremoved {
		stuck[key] = struct{}{}
	}
	expired := make([]uuid.UUID, 0, len(items))
	for _, item := range items {
		if _, blocked := stuck[item.ObjectKey]; !blocked {
			expired = append(expired, item.ID)
		}
	}

	if err := s.repo.DeleteManyUploads(ctx, expired); err != nil {
		return fmt.Errorf("expire %d stale uploads: %w", len(expired), err)
	}
	if len(unremoved) > 0 {
		return fmt.Errorf("expired %d of %d stale uploads: %w", len(expired), len(items), deleteErr)
	}
	return nil
}

type CursorPage struct {
	Data       []PublicView
	NextCursor string
	HasMore    bool
}

type GalleryPage struct {
	Data []PublicView
	// Counts is filled on the first page only. It aggregates every media row
	// of the event, and the totals cannot change between the pages of one
	// cursor walk, so recomputing it per page is pure cost. A nil value means
	// "unchanged", not "zero": a client keeps what the first page gave it.
	Counts     *GalleryCounts
	NextCursor string
	HasMore    bool
}

func validModerationStatus(status Status) bool {
	return status == StatusReady || status == StatusFeatured || status == StatusHidden
}

func uniqueMediaIDs(ids []uuid.UUID) []uuid.UUID {
	seen := make(map[uuid.UUID]struct{}, len(ids))
	unique := make([]uuid.UUID, 0, len(ids))
	for _, id := range ids {
		if id == uuid.Nil {
			continue
		}
		if _, exists := seen[id]; !exists {
			seen[id] = struct{}{}
			unique = append(unique, id)
		}
	}
	return unique
}

func (s *Service) UpdateStatus(ctx context.Context, eventID, mediaID uuid.UUID, status Status) (Media, error) {
	if !validModerationStatus(status) {
		return Media{}, fmt.Errorf("invalid media status")
	}
	updated, err := s.repo.UpdateStatus(ctx, eventID, mediaID, status)
	if err != nil {
		return Media{}, err
	}
	item := newItem(updated)
	s.notify(ctx, Change{Kind: ChangeModerated, EventID: eventID, IDs: []uuid.UUID{mediaID}, Status: status, Item: &item})
	return updated, nil
}

func (s *Service) UpdateStatuses(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID, status Status) error {
	if !validModerationStatus(status) {
		return fmt.Errorf("invalid media status")
	}
	ids = uniqueMediaIDs(ids)
	if len(ids) == 0 || len(ids) > 100 {
		return fmt.Errorf("between 1 and 100 media ids are required")
	}
	if err := s.repo.UpdateStatuses(ctx, eventID, ids, status); err != nil {
		return err
	}
	s.notify(ctx, Change{Kind: ChangeModerated, EventID: eventID, IDs: ids, Status: status})
	return nil
}

func (s *Service) DeleteForEvent(ctx context.Context, eventID, mediaID uuid.UUID) error {
	if mediaID == uuid.Nil {
		return ErrNotFound
	}
	if err := s.repo.DeleteForEvent(ctx, eventID, mediaID); err != nil {
		return err
	}
	s.notify(ctx, Change{Kind: ChangeDeleted, EventID: eventID, IDs: []uuid.UUID{mediaID}})
	return nil
}

func (s *Service) DeleteManyForEvent(ctx context.Context, eventID uuid.UUID, ids []uuid.UUID) error {
	ids = uniqueMediaIDs(ids)
	if len(ids) == 0 || len(ids) > 100 {
		return fmt.Errorf("between 1 and 100 media ids are required")
	}
	if err := s.repo.DeleteManyForEvent(ctx, eventID, ids); err != nil {
		return err
	}
	s.notify(ctx, Change{Kind: ChangeDeleted, EventID: eventID, IDs: ids})
	return nil
}

func (s *Service) ListReady(ctx context.Context, eventID uuid.UUID, limit int, cursor string, urlFor func(uuid.UUID) string) (CursorPage, error) {
	if limit < 1 || limit > 100 {
		limit = 60
	}
	var before *Cursor
	if cursor != "" {
		var c struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}
		raw, e := base64.RawURLEncoding.DecodeString(cursor)
		if e != nil || json.Unmarshal(raw, &c) != nil {
			return CursorPage{}, fmt.Errorf("invalid cursor")
		}
		before = &Cursor{CreatedAt: c.CreatedAt, ID: c.ID}
	}
	records, err := s.repo.ListReady(ctx, eventID, limit+1, before)
	if err != nil {
		return CursorPage{}, err
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	views := make([]PublicView, 0, len(records))
	for _, record := range records {
		views = append(views, newPublicView(record, urlFor))
	}
	page := CursorPage{Data: views, HasMore: hasMore}
	if hasMore {
		last := records[len(records)-1]
		raw, _ := json.Marshal(struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}{last.CreatedAt, last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

func (s *Service) ListGallery(ctx context.Context, eventID uuid.UUID, filter GalleryFilter, sort GallerySort, limit int, cursor string, urlFor func(uuid.UUID) string) (GalleryPage, error) {
	if limit < 1 || limit > 100 {
		limit = 24
	}
	if filter == "" {
		filter = FilterAll
	}
	if sort == "" {
		sort = SortNewest
	}
	if filter != FilterAll && filter != FilterPhotos && filter != FilterVideos && filter != FilterFavorites && filter != FilterHidden {
		return GalleryPage{}, fmt.Errorf("invalid media filter")
	}
	if sort != SortNewest && sort != SortOldest {
		return GalleryPage{}, fmt.Errorf("invalid media sort")
	}
	var before *Cursor
	if cursor != "" {
		var c struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursor)
		if err != nil || json.Unmarshal(raw, &c) != nil {
			return GalleryPage{}, fmt.Errorf("invalid cursor")
		}
		before = &Cursor{CreatedAt: c.CreatedAt, ID: c.ID}
	}
	records, err := s.repo.ListGallery(ctx, eventID, filter, sort, limit+1, before)
	if err != nil {
		return GalleryPage{}, err
	}
	var counts *GalleryCounts
	if before == nil {
		totals, countErr := s.repo.GalleryCounts(ctx, eventID)
		if countErr != nil {
			return GalleryPage{}, countErr
		}
		counts = &totals
	}
	hasMore := len(records) > limit
	if hasMore {
		records = records[:limit]
	}
	views := make([]PublicView, 0, len(records))
	for _, record := range records {
		views = append(views, newPublicView(record, urlFor))
	}
	page := GalleryPage{Data: views, Counts: counts, HasMore: hasMore}
	if hasMore {
		last := records[len(records)-1]
		raw, _ := json.Marshal(struct {
			CreatedAt time.Time `json:"created_at"`
			ID        uuid.UUID `json:"id"`
		}{last.CreatedAt, last.ID})
		page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
	}
	return page, nil
}

// newPublicView projects a stored record for a gallery response. The URL it
// starts with is the application route; SignPage replaces it with a direct
// object link wherever one can be issued.
func newPublicView(record Media, urlFor func(uuid.UUID) string) PublicView {
	route := urlFor(record.ID)
	return PublicView{
		ID:                   record.ID,
		URL:                  route,
		MIMEType:             record.MIMEType,
		OriginalFilename:     record.OriginalFilename,
		CreatedAt:            record.CreatedAt,
		IsVideo:              strings.HasPrefix(record.MIMEType, "video/"),
		HasEventFrame:        strings.HasPrefix(record.OriginalFilename, "candid_"),
		ThumbnailReady:       record.ThumbnailReady,
		Status:               record.Status,
		GuestName:            record.GuestName,
		Caption:              record.Caption,
		Width:                record.Width,
		Height:               record.Height,
		DurationSeconds:      record.DurationSeconds,
		TransitionCueSeconds: record.TransitionCueSeconds,
		objectKey:            record.ObjectKey,
		thumbnailKey:         thumbnailKey(record),
		routeURL:             route,
		sourceArchived:       record.SourceArchived(),
	}
}

// SignPage turns a page of views into links a browser can fetch directly.
//
// Without it every image in a gallery costs a round trip of its own: the
// client asks the API for /media/{id}/content, which looks up the event and
// the media row and then redirects. Sixty photos meant a hundred and twenty
// queries and sixty redirects for one screen, repeated on every scroll
// because the redirect is deliberately not cacheable.
//
// Signing is local HMAC work with no network call, so the whole page costs
// about as much as the query that produced it.
//
// Media whose original has been archived out of hot storage keeps its
// application route: only that route can stream the copy from the archive.
// Its thumbnail is still signed, because thumbnails are not archived.
func (s *Service) SignPage(ctx context.Context, views []PublicView, expiry time.Duration) error {
	for i := range views {
		if !views[i].sourceArchived && views[i].objectKey != "" {
			url, err := s.storage.PresignGet(ctx, views[i].objectKey, expiry)
			if err != nil {
				return err
			}
			views[i].URL = url

			if ds, ok := s.storage.(interface {
				PresignDownload(context.Context, string, string, time.Duration) (string, error)
			}); ok {
				filename := views[i].OriginalFilename
				if filename == "" {
					filename = fmt.Sprintf("candid-media-%s%s", views[i].ID.String()[:8], filepath.Ext(views[i].objectKey))
				}
				dlURL, err := ds.PresignDownload(ctx, views[i].objectKey, filename, expiry)
				if err == nil {
					views[i].DownloadURL = dlURL
				}
			}
		}
		if views[i].ThumbnailReady && views[i].thumbnailKey != "" {
			url, err := s.storage.PresignGet(ctx, views[i].thumbnailKey, expiry)
			if err != nil {
				return err
			}
			views[i].ThumbnailURL = url
		}
	}
	return nil
}

func (s *Service) ReadURL(ctx context.Context, eventID, mediaID uuid.UUID, expiry time.Duration) (string, error) {
	record, err := s.repo.FindReady(ctx, eventID, mediaID)
	if err != nil {
		return "", err
	}
	return s.storage.PresignGet(ctx, record.ObjectKey, expiry)
}

// ArchivedLocation returns the opaque Google Drive reference for an archived
// original. It is intentionally never serialized to API clients.
func (s *Service) ArchivedLocation(ctx context.Context, eventID, mediaID uuid.UUID) (string, error) {
	return s.repo.FindArchivedLocation(ctx, eventID, mediaID)
}

var supportedMIMETypes = map[string]string{
	"image/jpeg":      ".jpg",
	"image/heic":      ".heic",
	"image/png":       ".png",
	"image/webp":      ".webp",
	"video/mp4":       ".mp4",
	"video/quicktime": ".mov",
}

func allowed(mimeType string) bool {
	_, ok := supportedMIMETypes[mimeType]
	return ok
}

func limitByMIME(mimeType string, maxImageBytes, maxVideoBytes int64) int64 {
	if strings.HasPrefix(mimeType, "image/") {
		return maxImageBytes
	}
	return maxVideoBytes
}

func extension(mimeType string) string {
	return supportedMIMETypes[mimeType]
}

func validSHA256(value string) bool {
	if len(value) != 64 {
		return false
	}
	for _, char := range value {
		if !((char >= '0' && char <= '9') || (char >= 'a' && char <= 'f') || (char >= 'A' && char <= 'F')) {
			return false
		}
	}
	return true
}
