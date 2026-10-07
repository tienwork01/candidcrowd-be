package event

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type ListFilter struct {
	Page      int
	PerPage   int
	Query     string
	EventType string
	Sort      string
	Direction string
}

// ErrDuplicateRequest reports that the host already created an event with the
// same client request id.
var ErrDuplicateRequest = errors.New("event: duplicate create request")

// Provisioner attaches what every new event must start with — its plan grant
// — inside the event's insert transaction. A failure rolls the event back, so
// no event is ever committed half-provisioned.
type Provisioner interface {
	Provision(ctx context.Context, tx *gorm.DB, evt *Event) error
}

type Repository interface {
	Create(ctx context.Context, event *Event) error
	GetByClientRequest(ctx context.Context, hostID, clientRequestID uuid.UUID) (Event, error)
	ListByHost(ctx context.Context, hostID uuid.UUID, filter ListFilter) ([]Event, int64, error)
	GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error)
	GetPublic(ctx context.Context, slug string) (Event, error)
	UpdateOwned(ctx context.Context, id, hostID uuid.UUID, updates map[string]interface{}) (Event, error)
	DeleteOwned(ctx context.Context, id, hostID uuid.UUID) error
	CloseOwned(ctx context.Context, id, hostID uuid.UUID) error
	CountActiveTrialsByHost(ctx context.Context, hostID uuid.UUID) (int64, error)
	CountTrialsSince(ctx context.Context, hostID uuid.UUID, since time.Time) (int64, error)
}

type gormRepository struct {
	db           *gorm.DB
	provisioners []Provisioner
}

func NewGormRepository(db *gorm.DB, provisioners ...Provisioner) Repository {
	return &gormRepository{db: db, provisioners: provisioners}
}

func (r *gormRepository) Create(ctx context.Context, event *Event) error {
	err := r.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		if err := tx.Create(event).Error; err != nil {
			return err
		}
		for _, p := range r.provisioners {
			if err := p.Provision(ctx, tx, event); err != nil {
				return err
			}
		}
		return nil
	})
	if errors.Is(err, gorm.ErrDuplicatedKey) {
		if event.ClientRequestID != nil {
			return ErrDuplicateRequest
		}
		// The trial partial unique index is the final concurrency guard after
		// the service-level pre-check. Report the domain conflict, not a 500.
		return ErrActiveEventLimitReached
	}
	return err
}

func (r *gormRepository) GetByClientRequest(ctx context.Context, hostID, clientRequestID uuid.UUID) (Event, error) {
	var evt Event
	err := r.db.WithContext(ctx).Where("host_id = ? AND client_request_id = ?", hostID, clientRequestID).First(&evt).Error
	return evt, mapNotFound(err)
}

func (r *gormRepository) ListByHost(ctx context.Context, hostID uuid.UUID, filter ListFilter) ([]Event, int64, error) {
	db := r.db.WithContext(ctx).Model(&Event{}).Where("host_id = ? AND status <> ?", hostID, StatusDeleted)

	if q := strings.TrimSpace(filter.Query); q != "" {
		likePattern := "%" + q + "%"
		db = db.Where("name ILIKE ? OR event_type ILIKE ?", likePattern, likePattern)
	}

	if eventType := strings.TrimSpace(filter.EventType); eventType != "" {
		db = db.Where("LOWER(event_type) = LOWER(?)", eventType)
	}

	var total int64
	if err := db.Count(&total).Error; err != nil {
		return nil, 0, err
	}

	direction := strings.ToLower(strings.TrimSpace(filter.Direction))
	sortField := strings.ToLower(strings.TrimSpace(filter.Sort))

	switch sortField {
	case "name":
		if direction == "desc" {
			db = db.Order("name DESC")
		} else {
			db = db.Order("name ASC")
		}
	case "event_date", "upcoming":
		if direction == "desc" {
			db = db.Order("event_date DESC NULLS LAST, created_at DESC")
		} else {
			db = db.Order("CASE WHEN event_date IS NOT NULL AND event_date >= CURRENT_DATE THEN 0 ELSE 1 END, CASE WHEN event_date IS NOT NULL AND event_date >= CURRENT_DATE THEN event_date END ASC, event_date DESC, created_at DESC")
		}
	case "oldest":
		db = db.Order("created_at ASC")
	case "newest":
		db = db.Order("created_at DESC")
	default: // "created_at" or unspecified
		if direction == "asc" {
			db = db.Order("created_at ASC")
		} else {
			db = db.Order("created_at DESC")
		}
	}

	page := filter.Page
	if page < 1 {
		page = 1
	}
	perPage := filter.PerPage
	if perPage < 1 {
		perPage = 12
	}
	offset := (page - 1) * perPage

	events := make([]Event, 0)
	err := db.Offset(offset).Limit(perPage).Find(&events).Error
	if err != nil {
		return nil, 0, err
	}
	attachMetrics(ctx, r.db, events)
	return events, total, nil
}

func (r *gormRepository) GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error) {
	var evt Event
	err := r.db.WithContext(ctx).Where("id = ? AND host_id = ? AND status <> ?", id, hostID, StatusDeleted).First(&evt).Error
	if err != nil {
		return evt, mapNotFound(err)
	}
	evts := []Event{evt}
	attachMetrics(ctx, r.db, evts)
	return evts[0], nil
}

func (r *gormRepository) GetPublic(ctx context.Context, slug string) (Event, error) {
	var evt Event
	err := r.db.WithContext(ctx).Where("slug = ? AND status = ?", slug, StatusActive).First(&evt).Error
	return evt, mapNotFound(err)
}

func (r *gormRepository) UpdateOwned(ctx context.Context, id, hostID uuid.UUID, updates map[string]interface{}) (Event, error) {
	if len(updates) == 0 {
		return r.GetOwned(ctx, id, hostID)
	}
	// RETURNING reads back the row this statement wrote, rather than issuing a
	// second SELECT that a concurrent update could answer differently.
	var updated Event
	res := r.db.WithContext(ctx).Model(&updated).
		Clauses(clause.Returning{}).
		Where("id = ? AND host_id = ? AND status <> ?", id, hostID, StatusDeleted).
		Updates(updates)
	if res.Error != nil {
		return Event{}, res.Error
	}
	if res.RowsAffected == 0 {
		return Event{}, ErrNotFound
	}
	return updated, nil
}

func (r *gormRepository) DeleteOwned(ctx context.Context, id, hostID uuid.UUID) error {
	result := r.db.WithContext(ctx).Model(&Event{}).
		Where("id = ? AND host_id = ? AND status <> ?", id, hostID, StatusDeleted).
		Updates(map[string]interface{}{"status": StatusDeleted, "gallery_enabled": false})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *gormRepository) CloseOwned(ctx context.Context, id, hostID uuid.UUID) error {
	result := r.db.WithContext(ctx).Model(&Event{}).
		Where("id = ? AND host_id = ? AND status = ?", id, hostID, StatusActive).
		Updates(map[string]interface{}{"status": StatusClosed, "updated_at": time.Now().UTC()})
	if result.Error != nil {
		return result.Error
	}
	if result.RowsAffected != 1 {
		return ErrNotFound
	}
	return nil
}

func (r *gormRepository) CountActiveTrialsByHost(ctx context.Context, hostID uuid.UUID) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Event{}).
		Where("host_id = ? AND status = ? AND trial_started_at IS NOT NULL AND trial_ended_at IS NULL", hostID, StatusActive).
		Count(&count).Error
	return count, err
}

func (r *gormRepository) CountTrialsSince(ctx context.Context, hostID uuid.UUID, since time.Time) (int64, error) {
	var count int64
	err := r.db.WithContext(ctx).Model(&Event{}).
		Where("host_id = ? AND trial_started_at >= ?", hostID, since).
		Count(&count).Error
	return count, err
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

func attachMetrics(ctx context.Context, db *gorm.DB, events []Event) {
	if len(events) == 0 {
		return
	}
	eventIDs := make([]uuid.UUID, len(events))
	for i, e := range events {
		eventIDs[i] = e.ID
	}

	type mediaRow struct {
		EventID           uuid.UUID `gorm:"column:event_id"`
		ContributorsCount int64     `gorm:"column:contributors_count"`
		PhotosCount       int64     `gorm:"column:photos_count"`
		VideosCount       int64     `gorm:"column:videos_count"`
	}

	var mediaRows []mediaRow
	_ = db.WithContext(ctx).Raw(`
		SELECT event_id,
		       COUNT(DISTINCT guest_session_id) AS contributors_count,
		       COUNT(*) FILTER (WHERE mime_type LIKE 'image/%') AS photos_count,
		       COUNT(*) FILTER (WHERE mime_type LIKE 'video/%') AS videos_count
		FROM media
		WHERE event_id IN (?) AND status IN ('ready', 'featured', 'hidden')
		GROUP BY event_id`, eventIDs).Scan(&mediaRows)

	type sessionRow struct {
		EventID    uuid.UUID `gorm:"column:event_id"`
		ScansCount int64     `gorm:"column:scans_count"`
	}

	var sessionRows []sessionRow
	_ = db.WithContext(ctx).Raw(`
		SELECT event_id,
		       COUNT(*) AS scans_count
		FROM guest_sessions
		WHERE event_id IN (?)
		GROUP BY event_id`, eventIDs).Scan(&sessionRows)

	mediaMap := make(map[uuid.UUID]mediaRow, len(mediaRows))
	for _, row := range mediaRows {
		mediaMap[row.EventID] = row
	}

	sessionMap := make(map[uuid.UUID]sessionRow, len(sessionRows))
	for _, row := range sessionRows {
		sessionMap[row.EventID] = row
	}

	for i := range events {
		m := mediaMap[events[i].ID]
		s := sessionMap[events[i].ID]
		rate := 0
		if events[i].ExpectedGuestCount > 0 {
			rate = int(float64(m.ContributorsCount) / float64(events[i].ExpectedGuestCount) * 100)
			if rate > 100 {
				rate = 100
			}
		}
		events[i].Metrics = &Metrics{
			ScansCount:        s.ScansCount,
			VisitorsCount:     s.ScansCount,
			ContributorsCount: m.ContributorsCount,
			PhotosCount:       m.PhotosCount,
			VideosCount:       m.VideosCount,
			ParticipationRate: rate,
		}
	}
}
