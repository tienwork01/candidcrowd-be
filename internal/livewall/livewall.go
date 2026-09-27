package livewall

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

var (
	ErrNotFound             = errors.New("live wall: session not found")
	ErrInvalidCommand       = errors.New("live wall: invalid command")
	ErrInvalidContentPolicy = errors.New("live wall: invalid content policy")
	ErrInvalidCTAEveryMedia = errors.New("live wall: invalid CTA cadence")
)

type Status string
type ContentPolicy string

const (
	StatusLive    Status = "live"
	StatusEnded   Status = "ended"
	StatusRevoked Status = "revoked"
)

const (
	ContentPolicyFeaturedOnly ContentPolicy = "featured_only"
	ContentPolicyAutoApproved ContentPolicy = "auto_approved"
)

const (
	DefaultCTAEveryMedia = 8
	MinCTAEveryMedia     = 3
	MaxCTAEveryMedia     = 30
)

type Session struct {
	ID            uuid.UUID     `gorm:"type:uuid;primaryKey" json:"id"`
	EventID       uuid.UUID     `gorm:"type:uuid;not null;index" json:"event_id"`
	EventName     string        `gorm:"not null" json:"event_name"`
	EventSlug     string        `gorm:"not null" json:"event_slug"`
	TokenHash     string        `gorm:"not null;uniqueIndex" json:"-"`
	Status        Status        `gorm:"not null" json:"status"`
	ContentPolicy ContentPolicy `gorm:"not null;default:auto_approved" json:"content_policy"`
	IsPlaying     bool          `gorm:"not null;default:true" json:"is_playing"`
	ShowCTA       bool          `gorm:"not null;default:false" json:"show_cta"`
	IsBlackout    bool          `gorm:"not null;default:false" json:"is_blackout"`
	CTAEveryMedia int           `gorm:"not null;default:8" json:"cta_every_media"`
	Revision      int64         `gorm:"not null;default:0" json:"revision"`
	ExpiresAt     time.Time     `json:"expires_at"`
	CreatedAt     time.Time     `json:"created_at"`
	EndedAt       *time.Time    `json:"ended_at,omitempty"`
}

// TableName keeps GORM aligned with the explicit, domain-scoped migration
// table name. Without it GORM pluralizes Session to "sessions", which does
// not exist in the product schema.
func (Session) TableName() string {
	return "live_wall_sessions"
}

type Command string

const (
	CommandPlay     Command = "play"
	CommandPause    Command = "pause"
	CommandShowCTA  Command = "show_cta"
	CommandHideCTA  Command = "hide_cta"
	CommandNext     Command = "next"
	CommandPrevious Command = "previous"
	CommandBlackout Command = "blackout"
	CommandResume   Command = "resume"
)

type Change struct {
	Session Session
	Command Command
}

// Notifier is intentionally narrow so the live-wall package stays independent
// from the transport used to notify connected players.
type Notifier interface {
	LiveWallChanged(context.Context, Change)
}

type Repository interface {
	Create(context.Context, Session) (Session, error)
	FindOwned(context.Context, uuid.UUID, uuid.UUID) (Session, error)
	FindToken(context.Context, string, time.Time) (Session, error)
	End(context.Context, uuid.UUID, uuid.UUID, Status, time.Time) (Session, error)
	Command(context.Context, uuid.UUID, uuid.UUID, Command) (Session, error)
	UpdateContentPolicy(context.Context, uuid.UUID, uuid.UUID, ContentPolicy) (Session, error)
	UpdateCTAEveryMedia(context.Context, uuid.UUID, uuid.UUID, int) (Session, error)
}

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

func (r *gormRepository) Create(ctx context.Context, session Session) (Session, error) {
	err := r.db.WithContext(ctx).Create(&session).Error
	return session, err
}

func (r *gormRepository) FindOwned(ctx context.Context, eventID, id uuid.UUID) (Session, error) {
	var session Session
	err := r.db.WithContext(ctx).Where("id = ? AND event_id = ?", id, eventID).First(&session).Error
	return session, mapNotFound(err)
}

func (r *gormRepository) FindToken(ctx context.Context, hash string, now time.Time) (Session, error) {
	var session Session
	err := r.db.WithContext(ctx).Where("token_hash = ? AND status = ? AND expires_at > ?", hash, StatusLive, now).First(&session).Error
	return session, mapNotFound(err)
}

// updateLive applies one change to a live session and returns the updated row
// from the same statement.
//
// The host's remote drives these: play, pause, next, show the CTA. Each press
// used to cost an UPDATE followed by a SELECT to read back what it had just
// written. RETURNING makes it one round trip, and removes the window in which
// a concurrent command could be read back instead of this one.
func (r *gormRepository) updateLive(ctx context.Context, eventID, id uuid.UUID, updates map[string]any) (Session, error) {
	var session Session
	result := r.db.WithContext(ctx).Model(&session).
		Clauses(clause.Returning{}).
		Where("id = ? AND event_id = ? AND status = ?", id, eventID, StatusLive).
		Updates(updates)
	if result.Error != nil {
		return Session{}, result.Error
	}
	if result.RowsAffected != 1 {
		return Session{}, ErrNotFound
	}
	return session, nil
}

func (r *gormRepository) End(ctx context.Context, eventID, id uuid.UUID, status Status, endedAt time.Time) (Session, error) {
	return r.updateLive(ctx, eventID, id, map[string]any{"status": status, "ended_at": endedAt})
}

func (r *gormRepository) Command(ctx context.Context, eventID, id uuid.UUID, command Command) (Session, error) {
	updates := map[string]any{"revision": gorm.Expr("revision + 1")}
	switch command {
	case CommandPlay:
		updates["is_playing"] = true
	case CommandPause:
		updates["is_playing"] = false
	case CommandShowCTA:
		updates["show_cta"] = true
	case CommandHideCTA:
		updates["show_cta"] = false
	case CommandNext, CommandPrevious:
		// Playlist position belongs to each player, not the persistent session.
		// Revision still advances so every player receives this transport event.
	case CommandBlackout:
		updates["is_blackout"] = true
	case CommandResume:
		updates["is_blackout"] = false
	default:
		return Session{}, ErrInvalidCommand
	}
	return r.updateLive(ctx, eventID, id, updates)
}

func (r *gormRepository) UpdateContentPolicy(ctx context.Context, eventID, id uuid.UUID, policy ContentPolicy) (Session, error) {
	if policy != ContentPolicyFeaturedOnly && policy != ContentPolicyAutoApproved {
		return Session{}, ErrInvalidContentPolicy
	}
	return r.updateLive(ctx, eventID, id, map[string]any{"content_policy": policy, "revision": gorm.Expr("revision + 1")})
}

func (r *gormRepository) UpdateCTAEveryMedia(ctx context.Context, eventID, id uuid.UUID, every int) (Session, error) {
	return r.updateLive(ctx, eventID, id, map[string]any{"cta_every_media": every, "revision": gorm.Expr("revision + 1")})
}

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}

type Service struct {
	repo      Repository
	lifetime  time.Duration
	notifiers []Notifier
}

func NewService(repo Repository, lifetime time.Duration, notifiers ...Notifier) *Service {
	if lifetime <= 0 {
		lifetime = 12 * time.Hour
	}
	return &Service{repo: repo, lifetime: lifetime, notifiers: notifiers}
}

func (s *Service) Create(ctx context.Context, eventID uuid.UUID, eventName, eventSlug string) (Session, string, error) {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		return Session{}, "", err
	}
	token := base64.RawURLEncoding.EncodeToString(bytes)
	now := time.Now().UTC()
	// A fresh wall should show successfully uploaded guest media immediately.
	// Hosts can still switch to featured_only from the live controls when they
	// need a curated display.
	session := Session{ID: uuid.New(), EventID: eventID, EventName: eventName, EventSlug: eventSlug, TokenHash: tokenHash(token), Status: StatusLive, ContentPolicy: ContentPolicyAutoApproved, CTAEveryMedia: DefaultCTAEveryMedia, ExpiresAt: now.Add(s.lifetime), CreatedAt: now}
	created, err := s.repo.Create(ctx, session)
	return created, token, err
}

func (s *Service) GetOwned(ctx context.Context, eventID, id uuid.UUID) (Session, error) {
	return s.repo.FindOwned(ctx, eventID, id)
}
func (s *Service) GetPublic(ctx context.Context, token string) (Session, error) {
	return s.repo.FindToken(ctx, tokenHash(token), time.Now().UTC())
}
func (s *Service) End(ctx context.Context, eventID, id uuid.UUID) error {
	session, err := s.repo.End(ctx, eventID, id, StatusEnded, time.Now().UTC())
	if err == nil {
		s.notify(ctx, session, "")
	}
	return err
}
func (s *Service) Command(ctx context.Context, eventID, id uuid.UUID, command Command) (Session, error) {
	session, err := s.repo.Command(ctx, eventID, id, command)
	if err == nil {
		s.notify(ctx, session, command)
	}
	return session, err
}
func (s *Service) UpdateContentPolicy(ctx context.Context, eventID, id uuid.UUID, policy ContentPolicy) (Session, error) {
	session, err := s.repo.UpdateContentPolicy(ctx, eventID, id, policy)
	if err == nil {
		s.notify(ctx, session, "")
	}
	return session, err
}
func (s *Service) UpdateCTAEveryMedia(ctx context.Context, eventID, id uuid.UUID, every int) (Session, error) {
	if every < MinCTAEveryMedia || every > MaxCTAEveryMedia {
		return Session{}, ErrInvalidCTAEveryMedia
	}
	session, err := s.repo.UpdateCTAEveryMedia(ctx, eventID, id, every)
	if err == nil {
		s.notify(ctx, session, "")
	}
	return session, err
}
func (s *Service) Revoke(ctx context.Context, eventID, id uuid.UUID) error {
	session, err := s.repo.End(ctx, eventID, id, StatusRevoked, time.Now().UTC())
	if err == nil {
		s.notify(ctx, session, "")
	}
	return err
}

func (s *Service) notify(ctx context.Context, session Session, command Command) {
	for _, notifier := range s.notifiers {
		notifier.LiveWallChanged(ctx, Change{Session: session, Command: command})
	}
}

func tokenHash(token string) string {
	sum := sha256.Sum256([]byte(token))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}
