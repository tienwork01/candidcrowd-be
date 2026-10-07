package catalog

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"gorm.io/gorm"
)

type Repository interface {
	ActiveVersions(ctx context.Context) ([]Version, error)
	ActiveVersion(ctx context.Context, code PlanCode) (Version, error)
	// CurrentPrices returns, per version, the newest price in currency valid
	// at the given instant.
	CurrentPrices(ctx context.Context, versionIDs []uuid.UUID, currency string, at time.Time) ([]Price, error)
}

type gormRepository struct{ db *gorm.DB }

func NewGormRepository(db *gorm.DB) Repository { return &gormRepository{db: db} }

func (r *gormRepository) ActiveVersions(ctx context.Context) ([]Version, error) {
	var versions []Version
	err := r.db.WithContext(ctx).Where("status = ?", VersionActive).Find(&versions).Error
	return versions, err
}

func (r *gormRepository) ActiveVersion(ctx context.Context, code PlanCode) (Version, error) {
	var v Version
	err := r.db.WithContext(ctx).Where("code = ? AND status = ?", code, VersionActive).First(&v).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return v, ErrNotFound
	}
	return v, err
}

func (r *gormRepository) CurrentPrices(ctx context.Context, versionIDs []uuid.UUID, currency string, at time.Time) ([]Price, error) {
	if len(versionIDs) == 0 {
		return nil, nil
	}
	var prices []Price
	err := r.db.WithContext(ctx).Raw(`
		SELECT DISTINCT ON (plan_version_id) *
		FROM plan_prices
		WHERE plan_version_id IN ?
		  AND currency = ?
		  AND valid_from <= ?
		  AND (valid_until IS NULL OR valid_until > ?)
		ORDER BY plan_version_id, valid_from DESC`,
		versionIDs, currency, at, at).Scan(&prices).Error
	return prices, err
}
