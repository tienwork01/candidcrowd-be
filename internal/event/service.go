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

// Change describes event settings a connected browser reacts to. A guest page
// must learn that the gallery was switched off or the mode changed without
// being reloaded.
type Change struct {
	EventID        uuid.UUID `json:"-"`
	GalleryEnabled bool      `json:"gallery_enabled"`
	EventMode      string    `json:"event_mode"`
	Status         Status    `json:"status"`
}

// Notifier receives changes for broadcast. Implementations must return
// promptly; any network work belongs out of band.
type Notifier interface {
	EventChanged(ctx context.Context, change Change)
}

type Service struct {
	repo          Repository
	eventMaxBytes int64
	notifier      Notifier
}

// NewService takes an optional Notifier so realtime broadcast stays a
// deployment concern rather than a precondition for managing events.
func NewService(repo Repository, eventMaxBytes int64, notifiers ...Notifier) *Service {
	s := &Service{repo: repo, eventMaxBytes: eventMaxBytes}
	if len(notifiers) > 0 {
		s.notifier = notifiers[0]
	}
	return s
}

type CreateInput struct {
	Name, EventType    string
	EventDate          *time.Time
	ExpectedGuestCount int
	ClientRequestID    *uuid.UUID
}

type UpdateInput struct {
	Name                *string
	EventType           *string
	EventDate           *time.Time
	ClearEventDate      bool
	ExpectedGuestCount  *int
	GalleryEnabled      *bool
	EventMode           *string
	LifecyclePhase      *string
	SetupChecklist      *datatypes.JSON
	CandidCameraEnabled *bool
	GuestTheme          *datatypes.JSON
	QRConfig            *datatypes.JSON
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
		HostID:              hostID,
		Name:                strings.TrimSpace(in.Name),
		Slug:                slug(),
		EventDate:           in.EventDate,
		EventType:           defaultType(in.EventType),
		ExpectedGuestCount:  in.ExpectedGuestCount,
		Status:              StatusActive,
		GalleryEnabled:      true,
		EventMode:           "social",
		SetupChecklist:      datatypes.JSON([]byte("{}")),
		CandidCameraEnabled: true,
		MaxMediaBytes:       s.eventMaxBytes,
		ClientRequestID:     in.ClientRequestID,
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

func (s *Service) Delete(ctx context.Context, id, hostID uuid.UUID) error {
	return s.repo.DeleteOwned(ctx, id, hostID)
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
	if in.EventMode != nil {
		updates["event_mode"] = strings.TrimSpace(*in.EventMode)
	}
	if in.LifecyclePhase != nil {
		updates["lifecycle_phase"] = strings.TrimSpace(*in.LifecyclePhase)
	}
	if in.SetupChecklist != nil {
		updates["setup_checklist"] = in.SetupChecklist
	}
	if in.CandidCameraEnabled != nil {
		updates["candid_camera_enabled"] = *in.CandidCameraEnabled
	}
	if in.GuestTheme != nil {
		updates["guest_theme"] = in.GuestTheme
	}
	if in.QRConfig != nil {
		if err := validateQRConfig(*in.QRConfig); err != nil {
			return Event{}, err
		}
		updates["qr_config"] = in.QRConfig
	}
	if len(updates) > 0 {
		updates["updated_at"] = time.Now().UTC()
	}
	updated, err := s.repo.UpdateOwned(ctx, id, hostID, updates)
	if err != nil {
		return Event{}, err
	}
	// Only settings a live page reacts to are broadcast. Renaming an event or
	// editing its checklist changes nothing a connected browser must redraw.
	if s.notifier != nil && (in.GalleryEnabled != nil || in.EventMode != nil) {
		s.notifier.EventChanged(ctx, Change{
			EventID:        updated.ID,
			GalleryEnabled: updated.GalleryEnabled,
			EventMode:      updated.EventMode,
			Status:         updated.Status,
		})
	}
	return updated, nil
}

func defaultType(eventType string) string {
	if strings.TrimSpace(eventType) == "" {
		return "other"
	}
	return strings.TrimSpace(eventType)
}

func slug() string { return "evt_" + strings.ReplaceAll(uuid.NewString(), "-", "")[:12] }
