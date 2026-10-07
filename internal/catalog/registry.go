package catalog

import (
	"context"
	"fmt"
	"sort"
	"sync/atomic"

	"gorm.io/gorm"
)

// PlanDefinition is one plan in the registry: what it is called, where it sits
// in the upgrade order, and whether it can be sold.
//
// It is deliberately not a Version. A version says what a plan grants and
// changes over time; this says that the plan exists at all.
type PlanDefinition struct {
	Code      PlanCode `gorm:"column:code"`
	TierRank  int      `gorm:"column:tier_rank"`
	Sellable  bool     `gorm:"column:sellable"`
	RetiredAt *string  `gorm:"column:retired_at"`
}

func (PlanDefinition) TableName() string { return "plans" }

type planRegistry struct {
	byCode  map[PlanCode]PlanDefinition
	ordered []PlanDefinition
}

// registry is process-wide because a plan catalog is process-wide: it is read
// on nearly every request and written once at startup. The atomic pointer keeps
// reads lock-free and makes a reload a single swap.
var registry atomic.Pointer[planRegistry]

func init() {
	// The compiled-in default is the catalog this code was written against, so
	// tests, planctl and a boot against a database that predates the registry
	// table all behave exactly as before.
	SetRegistry([]PlanDefinition{
		{Code: PlanFree, TierRank: 0, Sellable: false},
		{Code: PlanExperience, TierRank: 1, Sellable: true},
		{Code: PlanSignature, TierRank: 2, Sellable: true},
	})
}

// SetRegistry replaces the known plans. It is used at startup and by tests that
// need a catalog this binary has never heard of.
func SetRegistry(defs []PlanDefinition) {
	next := &planRegistry{byCode: make(map[PlanCode]PlanDefinition, len(defs))}
	for _, def := range defs {
		if def.RetiredAt != nil {
			continue
		}
		next.byCode[def.Code] = def
		next.ordered = append(next.ordered, def)
	}
	sort.Slice(next.ordered, func(i, j int) bool { return next.ordered[i].TierRank < next.ordered[j].TierRank })
	registry.Store(next)
}

// LoadRegistry reads the plans table and replaces the in-process registry.
//
// A failure is reported but never fatal: the compiled-in default still serves
// the plans this binary knows, and refusing to boot over a catalog read would
// take the whole product down to protect a feature flag.
func LoadRegistry(ctx context.Context, db *gorm.DB) error {
	var defs []PlanDefinition
	if err := db.WithContext(ctx).Where("retired_at IS NULL").Find(&defs).Error; err != nil {
		return fmt.Errorf("catalog: load plan registry: %w", err)
	}
	if len(defs) == 0 {
		return fmt.Errorf("catalog: plan registry is empty")
	}
	SetRegistry(defs)
	return nil
}

// Plans lists every live plan, cheapest tier first.
func Plans() []PlanDefinition {
	return registry.Load().ordered
}

// SellablePlans lists the plans a host can actually buy, cheapest first. Every
// loop that used to name Experience and Signature asks this instead.
func SellablePlans() []PlanCode {
	reg := registry.Load()
	codes := make([]PlanCode, 0, len(reg.ordered))
	for _, def := range reg.ordered {
		if def.Sellable {
			codes = append(codes, def.Code)
		}
	}
	return codes
}

// Sellable reports whether a plan can be purchased.
func (c PlanCode) Sellable() bool {
	def, ok := registry.Load().byCode[c]
	return ok && def.Sellable
}

func (c PlanCode) Valid() bool {
	_, ok := registry.Load().byCode[c]
	return ok
}

// Rank is the plan's position in the upgrade order, or -1 for an unknown code.
func (c PlanCode) Rank() int {
	def, ok := registry.Load().byCode[c]
	if !ok {
		return -1
	}
	return def.TierRank
}
