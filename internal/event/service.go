package event

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/user"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Service struct {
	db            *gorm.DB
	eventMaxBytes int64
}

func NewService(db *gorm.DB, eventMaxBytes int64) *Service { return &Service{db, eventMaxBytes} }

type CreateInput struct {
	Name, EventType    string
	EventDate          *time.Time
	ExpectedGuestCount int
}

func (s *Service) Create(ctx context.Context, externalID, email string, in CreateInput) (Event, error) {
	if strings.TrimSpace(in.Name) == "" {
		return Event{}, fmt.Errorf("event name is required")
	}
	if in.ExpectedGuestCount < 0 {
		return Event{}, fmt.Errorf("expected guest count cannot be negative")
	}
	var u user.User
	err := s.db.WithContext(ctx).Where("better_auth_user_id = ?", externalID).FirstOrCreate(&u, user.User{BetterAuthUserID: externalID, Email: email}).Error
	if err != nil {
		return Event{}, err
	}
	e := Event{HostID: u.ID, Name: strings.TrimSpace(in.Name), Slug: slug(), EventDate: in.EventDate, EventType: defaultType(in.EventType), ExpectedGuestCount: in.ExpectedGuestCount, Status: StatusActive, GalleryEnabled: true, MaxMediaBytes: s.eventMaxBytes}
	return e, s.db.WithContext(ctx).Create(&e).Error
}
func (s *Service) List(ctx context.Context, externalID string) ([]Event, error) {
	var u user.User
	if e := s.db.WithContext(ctx).Where("better_auth_user_id = ?", externalID).First(&u).Error; e != nil {
		if e == gorm.ErrRecordNotFound {
			return []Event{}, nil
		}
		return nil, e
	}
	var events []Event
	e := s.db.WithContext(ctx).Where("host_id = ?", u.ID).Order("created_at desc").Find(&events).Error
	return events, e
}
func (s *Service) GetOwned(ctx context.Context, id uuid.UUID, externalID string) (Event, error) {
	var e Event
	err := s.db.WithContext(ctx).Joins("JOIN users ON users.id = events.host_id").Where("events.id = ? AND users.better_auth_user_id = ?", id, externalID).First(&e).Error
	return e, err
}
func (s *Service) GetPublic(ctx context.Context, slug string) (Event, error) {
	var e Event
	err := s.db.WithContext(ctx).Where("slug = ? AND status = ?", slug, StatusActive).First(&e).Error
	return e, err
}
func defaultType(v string) string {
	if strings.TrimSpace(v) == "" {
		return "other"
	}
	return strings.TrimSpace(v)
}
func slug() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
