package event

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"gorm.io/datatypes"
)

type Service struct {
	repo          Repository
	eventMaxBytes int64
}

func NewService(repo Repository, eventMaxBytes int64) *Service {
	return &Service{repo: repo, eventMaxBytes: eventMaxBytes}
}

type CreateInput struct {
	Name, EventType    string
	EventDate          *time.Time
	ExpectedGuestCount int
}

type UpdateInput struct {
	Name               *string
	EventType          *string
	EventDate          *time.Time
	ClearEventDate     bool
	ExpectedGuestCount *int
	GalleryEnabled     *bool
	GuestTheme         *datatypes.JSON
}

type ListInput struct {
	Page      int
	PerPage   int
	Query     string
	EventType string
	Sort      string
	Direction string
}

type ListOutput struct {
	Events     []Event
	Pagination Pagination
}

func (s *Service) Create(ctx context.Context, hostID uuid.UUID, in CreateInput) (Event, error) {
	if strings.TrimSpace(in.Name) == "" {
		return Event{}, fmt.Errorf("event name is required")
	}
	if in.ExpectedGuestCount < 0 {
		return Event{}, fmt.Errorf("expected guest count cannot be negative")
	}
	evt := Event{
		HostID:             hostID,
		Name:               strings.TrimSpace(in.Name),
		Slug:               slug(),
		EventDate:          in.EventDate,
		EventType:          defaultType(in.EventType),
		ExpectedGuestCount: in.ExpectedGuestCount,
		Status:             StatusActive,
		GalleryEnabled:     true,
		MaxMediaBytes:      s.eventMaxBytes,
	}
	if err := s.repo.Create(ctx, &evt); err != nil {
		return Event{}, err
	}
	return evt, nil
}

func (s *Service) List(ctx context.Context, hostID uuid.UUID, in ListInput) (ListOutput, error) {
	page := in.Page
	if page < 1 {
		page = 1
	}
	perPage := in.PerPage
	if perPage < 1 {
		perPage = 12
	} else if perPage > 100 {
		perPage = 100
	}

	filter := ListFilter{
		Page:      page,
		PerPage:   perPage,
		Query:     strings.TrimSpace(in.Query),
		EventType: strings.TrimSpace(in.EventType),
		Sort:      strings.TrimSpace(in.Sort),
		Direction: strings.TrimSpace(in.Direction),
	}

	events, total, err := s.repo.ListByHost(ctx, hostID, filter)
	if err != nil {
		return ListOutput{}, err
	}

	totalPages := 1
	if total > 0 {
		totalPages = int(math.Ceil(float64(total) / float64(perPage)))
	}

	pagination := Pagination{
		Page:       page,
		PerPage:    perPage,
		Total:      total,
		TotalPages: totalPages,
		HasNext:    page < totalPages,
		HasPrev:    page > 1,
	}

	return ListOutput{
		Events:     events,
		Pagination: pagination,
	}, nil
}

func (s *Service) GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error) {
	return s.repo.GetOwned(ctx, id, hostID)
}

func (s *Service) GetPublic(ctx context.Context, slug string) (Event, error) {
	return s.repo.GetPublic(ctx, slug)
}

func (s *Service) Update(ctx context.Context, id, hostID uuid.UUID, in UpdateInput) (Event, error) {
	updates := make(map[string]interface{})
	if in.Name != nil {
		trimmed := strings.TrimSpace(*in.Name)
		if trimmed == "" {
			return Event{}, fmt.Errorf("event name cannot be empty")
		}
		updates["name"] = trimmed
	}
	if in.EventType != nil {
		updates["event_type"] = defaultType(*in.EventType)
	}
	if in.ClearEventDate {
		updates["event_date"] = nil
	} else if in.EventDate != nil {
		updates["event_date"] = in.EventDate
	}
	if in.ExpectedGuestCount != nil {
		if *in.ExpectedGuestCount < 0 {
			return Event{}, fmt.Errorf("expected guest count cannot be negative")
		}
		updates["expected_guest_count"] = *in.ExpectedGuestCount
	}
	if in.GalleryEnabled != nil {
		updates["gallery_enabled"] = *in.GalleryEnabled
	}
	if in.GuestTheme != nil {
		updates["guest_theme"] = in.GuestTheme
	}
	if len(updates) > 0 {
		updates["updated_at"] = time.Now().UTC()
	}
	return s.repo.UpdateOwned(ctx, id, hostID, updates)
}

func defaultType(eventType string) string {
	if strings.TrimSpace(eventType) == "" {
		return "other"
	}
	return strings.TrimSpace(eventType)
}

func slug() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
