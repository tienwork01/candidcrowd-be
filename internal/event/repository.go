package event

import (
	"context"
	"errors"
	"strings"

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

type Repository interface {
	Create(ctx context.Context, event *Event) error
	ListByHost(ctx context.Context, hostID uuid.UUID, filter ListFilter) ([]Event, int64, error)
	GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error)
	GetPublic(ctx context.Context, slug string) (Event, error)
	UpdateOwned(ctx context.Context, id, hostID uuid.UUID, updates map[string]interface{}) (Event, error)
	DeleteOwned(ctx context.Context, id, hostID uuid.UUID) error
}

type gormRepository struct {
	db *gorm.DB
}

func NewGormRepository(db *gorm.DB) Repository {
	return &gormRepository{db: db}
}

func (r *gormRepository) Create(ctx context.Context, event *Event) error {
	return r.db.WithContext(ctx).Create(event).Error
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
	return events, total, nil
}

func (r *gormRepository) GetOwned(ctx context.Context, id, hostID uuid.UUID) (Event, error) {
	var evt Event
	err := r.db.WithContext(ctx).Where("id = ? AND host_id = ? AND status <> ?", id, hostID, StatusDeleted).First(&evt).Error
	return evt, mapNotFound(err)
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

func mapNotFound(err error) error {
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return ErrNotFound
	}
	return err
}
