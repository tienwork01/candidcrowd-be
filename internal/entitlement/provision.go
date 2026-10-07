package entitlement

import (
	"context"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/event"
	"github.com/google/uuid"
	"gorm.io/gorm"
)

// Provisioner gives every new event its Free grant inside the event's own
// insert transaction, so no event ever exists without a plan.
type Provisioner struct {
	catalog PlanCatalog
	now     func() time.Time
}

var _ event.Provisioner = (*Provisioner)(nil)

func NewProvisioner(plans PlanCatalog) *Provisioner {
	return &Provisioner{catalog: plans, now: time.Now}
}

func (p *Provisioner) Provision(ctx context.Context, tx *gorm.DB, evt *event.Event) error {
	version, err := p.catalog.ActiveVersion(ctx, catalog.PlanFree)
	if err != nil {
		return err
	}
	now := p.now().UTC()
	upload, retention := Expiry(evt.EventDate, now, version.Entitlements.Limits.RetentionDays)
	grant := Grant{
		ID:                  uuid.New(),
		EventID:             evt.ID,
		PlanVersionID:       version.ID,
		Status:              StatusActive,
		Source:              SourceSystem,
		EntitlementSnapshot: version.Entitlements,
		ActivatedAt:         now,
		UploadExpiresAt:     upload,
		RetentionExpiresAt:  retention,
		CreatedAt:           now,
	}
	if err := writeGrant(tx.WithContext(ctx), grant, nil, catalog.PlanFree, "event created"); err != nil {
		return err
	}
	if bytes := version.Entitlements.Limits.MaxMediaBytes; bytes > 0 {
		evt.MaxMediaBytes = bytes
	}
	return nil
}
