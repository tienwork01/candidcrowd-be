// Command planctl is the internal operations tool for event plans. It needs
// direct database access (DATABASE_URL) and is never exposed over HTTP. Every
// change it makes goes through entitlement.GrantActivator and is audited.
//
//	planctl show --event <id>
//	planctl grant --event <id> --plan experience|signature --source-ref <ref> --reason <text>
//	planctl revoke --event <id> --reason <text>
//	planctl reconcile
//	planctl retention [--execute]
//	planctl backfill-legacy --created-before 2026-10-04T12:00:00Z [--dry-run]
//	planctl product-map set --plan experience --product-id pro_...
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"strings"
	"time"

	"github.com/candidcrowd/candidcrowd-backend/internal/billing"
	"github.com/candidcrowd/candidcrowd-backend/internal/catalog"
	"github.com/candidcrowd/candidcrowd-backend/internal/entitlement"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/database"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/googledrive"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/r2"
	"github.com/candidcrowd/candidcrowd-backend/internal/platform/redis"
	"github.com/candidcrowd/candidcrowd-backend/internal/realtime"
	"github.com/candidcrowd/candidcrowd-backend/internal/retention"
	"github.com/google/uuid"
	"github.com/joho/godotenv"
	"gorm.io/gorm"
)

const usage = `usage: planctl <command> [flags]

commands:
  show              print an event's plan, limits, usage and plan history
  grant             activate Experience or Signature for one event
  revoke            undo an event's latest plan activation
  reconcile         check plan, quota and retention invariants across events
  retention         list (or with --execute, delete) media past its storage period
  backfill-legacy   move events created before a cutoff onto Experience
  price-map         manage optional fixed provider price mappings (list, set, retire)
  product-map       manage provider product anchors used for inline pricing
  billing-reconcile retry entitlement application for settled purchases
  billing-show      inspect billing purchases, provider events and grants
`

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, "planctl:", err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		fmt.Fprint(out, usage)
		return fmt.Errorf("missing command")
	}
	switch args[0] {
	case "show":
		return show(args[1:], out)
	case "grant":
		return grant(args[1:], out)
	case "revoke":
		return revoke(args[1:], out)
	case "reconcile":
		return reconcile(args[1:], out)
	case "retention":
		return retentionRun(args[1:], out)
	case "backfill-legacy":
		return backfillLegacy(args[1:], out)
	case "price-map":
		return priceMap(args[1:], out)
	case "product-map":
		return productMap(args[1:], out)
	case "billing-reconcile":
		return billingReconcile(args[1:], out)
	case "billing-show":
		return billingShow(args[1:], out)
	default:
		fmt.Fprint(out, usage)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

type deps struct {
	db      *gorm.DB
	plans   *catalog.Service
	reader  *entitlement.Service
	grants  *entitlement.GrantService
	closers []func()
}

func (d deps) Close() {
	for i := len(d.closers) - 1; i >= 0; i-- {
		d.closers[i]()
	}
}

// open wires the services against DATABASE_URL. When REDIS_URL is set, grants
// are also announced to open host screens.
func open() (deps, error) {
	_ = godotenv.Load()
	url := os.Getenv("DATABASE_URL")
	if url == "" {
		return deps{}, fmt.Errorf("DATABASE_URL is not set")
	}
	db, sqlDB, err := database.Open(url, 4, 2, time.Minute)
	if err != nil {
		return deps{}, err
	}
	d := deps{db: db, closers: []func(){func() { _ = sqlDB.Close() }}}
	// Every plan check below (sellable, rank, upgrade order) reads the registry,
	// so it has to come from the same database the command is about to change.
	if regErr := catalog.LoadRegistry(context.Background(), db); regErr != nil {
		fmt.Fprintln(os.Stderr, "planctl: using the built-in plan registry:", regErr)
	}
	d.plans = catalog.NewService(catalog.NewGormRepository(db))
	repo := entitlement.NewGormRepository(db)
	d.reader = entitlement.NewService(repo, d.plans)
	d.grants = entitlement.NewGrantService(repo, d.plans)

	if redisURL := os.Getenv("REDIS_URL"); redisURL != "" {
		bus, busErr := redis.OpenBus(redisURL)
		if busErr != nil {
			d.Close()
			return deps{}, busErr
		}
		d.closers = append(d.closers, func() { _ = bus.Close() })
		hub := realtime.NewHub(bus, slog.Default(), 0)
		d.closers = append(d.closers, hub.Close)
		d.grants.UseNotifier(realtime.SyncPlanNotifier{Hub: hub, Log: slog.Default()})
	}
	return d, nil
}

func parseEventID(raw string) (uuid.UUID, error) {
	if raw == "" {
		return uuid.Nil, fmt.Errorf("--event is required")
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		return uuid.Nil, fmt.Errorf("--event must be a UUID")
	}
	return id, nil
}

func show(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("show", flag.ContinueOnError)
	eventFlag := fs.String("event", "", "event id (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	eventID, err := parseEventID(*eventFlag)
	if err != nil {
		return err
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	ent, err := d.reader.Resolve(ctx, eventID)
	if errors.Is(err, entitlement.ErrNoActiveGrant) {
		fmt.Fprintf(out, "event %s has no active plan\n", eventID)
		return nil
	}
	if err != nil {
		return err
	}
	usage, err := d.reader.Usage(ctx, eventID)
	if err != nil {
		return err
	}
	l := ent.Limits()
	fmt.Fprintf(out, "event      %s\n", eventID)
	fmt.Fprintf(out, "plan       %s v%d (source %s)\n", ent.PlanCode, ent.PlanVersion, ent.Source)
	fmt.Fprintf(out, "features   %s\n", featureList(ent.Entitlements.Features))
	fmt.Fprintf(out, "uploads    open until %s\n", ent.UploadExpiresAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "retention  until %s\n", ent.RetentionExpiresAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(out, "media      %d / %s\n", usage.MediaItems, limit(l.MaxMediaItems))
	fmt.Fprintf(out, "photos     %d / %s\n", usage.PhotoItems, limit(l.MaxPhotoItems))
	fmt.Fprintf(out, "videos     %d / %s\n", usage.VideoItems, limit(l.MaxVideoItems))
	fmt.Fprintf(out, "bytes      %d / %d\n", usage.MediaBytes, l.MaxMediaBytes)

	trail, err := d.reader.AuditTrail(ctx, eventID, 20)
	if err != nil {
		return err
	}
	fmt.Fprintln(out, "history")
	for _, entry := range trail {
		from := "—"
		if entry.PreviousPlan != nil {
			from = *entry.PreviousPlan
		}
		fmt.Fprintf(out, "  %s  %s → %s  %s %s  %s\n",
			entry.CreatedAt.UTC().Format(time.RFC3339), from, entry.TargetPlan,
			entry.Source, deref(entry.SourceReference), deref(entry.Reason))
	}
	return nil
}

func grant(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("grant", flag.ContinueOnError)
	eventFlag := fs.String("event", "", "event id (required)")
	planFlag := fs.String("plan", "", "experience or signature (required)")
	sourceRef := fs.String("source-ref", "", "unique reference for this grant, e.g. a support ticket (required)")
	reason := fs.String("reason", "", "why the plan is granted; stored in the audit trail (required)")
	source := fs.String("source", string(entitlement.SourceManual), "manual or promotion")
	if err := fs.Parse(args); err != nil {
		return err
	}
	eventID, err := parseEventID(*eventFlag)
	if err != nil {
		return err
	}
	plan := catalog.PlanCode(strings.ToLower(strings.TrimSpace(*planFlag)))
	if !plan.Sellable() {
		return fmt.Errorf("--plan must be a sellable plan")
	}
	src := entitlement.Source(*source)
	if src != entitlement.SourceManual && src != entitlement.SourcePromotion {
		return fmt.Errorf("--source must be manual or promotion")
	}
	if strings.TrimSpace(*sourceRef) == "" {
		return fmt.Errorf("--source-ref is required")
	}
	if strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("--reason is required")
	}

	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	created, err := d.grants.Activate(ctx, entitlement.ActivateGrantInput{
		EventID:        eventID,
		TargetPlanCode: plan,
		Source:         src,
		SourceRef:      *sourceRef,
		// The operator is recorded with the reason: operators are not
		// CandidCrowd users, so there is no user id to store as the actor.
		Reason: fmt.Sprintf("[%s] %s", operator(), strings.TrimSpace(*reason)),
	})
	switch {
	case errors.Is(err, entitlement.ErrEventNotFound):
		return fmt.Errorf("event %s not found", eventID)
	case errors.Is(err, entitlement.ErrInvalidTransition):
		return fmt.Errorf("%w (plans only move up: free → experience → signature)", err)
	case errors.Is(err, entitlement.ErrDuplicateSourceReference):
		return fmt.Errorf("source reference %q was already used; nothing changed", *sourceRef)
	case err != nil:
		return err
	}
	fmt.Fprintf(out, "granted %s to event %s (grant %s)\n", plan, eventID, created.ID)
	fmt.Fprintf(out, "uploads open until %s, retention until %s\n",
		created.UploadExpiresAt.UTC().Format(time.RFC3339), created.RetentionExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

func backfillLegacy(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("backfill-legacy", flag.ContinueOnError)
	createdBefore := fs.String("created-before", "", "RFC 3339 cutoff; events created before it are moved to Experience (required)")
	dryRun := fs.Bool("dry-run", false, "count candidates without writing")
	batch := fs.Int("batch", 200, "events read per page")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *createdBefore == "" {
		return fmt.Errorf("--created-before is required")
	}
	cutoff, err := time.Parse(time.RFC3339, *createdBefore)
	if err != nil {
		return fmt.Errorf("--created-before: %w", err)
	}

	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()
	report, err := d.grants.BackfillLegacy(ctx, cutoff, *dryRun, *batch)

	mode := "upgraded"
	if *dryRun {
		mode = "would upgrade"
	}
	fmt.Fprintf(out, "%s: %d, already granted: %d, failed: %d\n", mode, report.Upgraded, report.Skipped, len(report.Failed))
	for _, f := range report.Failed {
		fmt.Fprintf(out, "  %s: %v\n", f.EventID, f.Err)
	}
	if err != nil {
		return err
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d events failed; re-run after fixing them", len(report.Failed))
	}
	return nil
}

func operator() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	return "unknown-operator"
}

func featureList(features []catalog.Feature) string {
	if len(features) == 0 {
		return "(none)"
	}
	names := make([]string, len(features))
	for i, f := range features {
		names[i] = string(f)
	}
	return strings.Join(names, ", ")
}

func limit(v int64) string {
	if v <= 0 {
		return "no limit"
	}
	return fmt.Sprint(v)
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

func revoke(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("revoke", flag.ContinueOnError)
	eventFlag := fs.String("event", "", "event id (required)")
	reason := fs.String("reason", "", "why the grant is revoked; stored in the audit trail (required)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	eventID, err := parseEventID(*eventFlag)
	if err != nil {
		return err
	}
	if strings.TrimSpace(*reason) == "" {
		return fmt.Errorf("--reason is required")
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	revoked, restored, err := d.grants.Revoke(ctx, eventID, fmt.Sprintf("[%s] %s", operator(), strings.TrimSpace(*reason)))
	switch {
	case errors.Is(err, entitlement.ErrEventNotFound):
		return fmt.Errorf("event %s not found", eventID)
	case errors.Is(err, entitlement.ErrNothingToRestore):
		return fmt.Errorf("event %s is on its first plan; there is nothing to go back to", eventID)
	case err != nil:
		return err
	}
	fmt.Fprintf(out, "revoked %s; event %s is back on %s\n", revoked.PlanCode, eventID, restored.PlanCode)
	fmt.Fprintf(out, "uploads open until %s, retention until %s\n",
		restored.UploadExpiresAt.UTC().Format(time.RFC3339), restored.RetentionExpiresAt.UTC().Format(time.RFC3339))
	return nil
}

func reconcile(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("reconcile", flag.ContinueOnError)
	grace := fs.Duration("grace", retention.DefaultGrace, "deletion grace period after a storage period ends")
	soon := fs.Duration("expiring-within", 7*24*time.Hour, "report storage periods ending within this window")
	sample := fs.Int("sample", 10, "event ids listed per finding")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	report, err := d.reader.Reconcile(ctx, *grace, *soon, *sample)
	if err != nil {
		return err
	}
	rows := []struct {
		name    string
		issue   entitlement.ReconcileIssue
		problem bool
	}{
		{"events without an active plan", report.EventsWithoutGrant, true},
		{"limits differ from the plan", report.LimitMismatch, true},
		{"counters differ from media", report.CounterDrift, true},
		{"due for retention purge", report.RetentionDue, false},
		{"storage ending soon", report.ExpiringSoon, false},
		{"awaiting billing review", report.BillingReview, false},
	}
	problems := int64(0)
	for _, row := range rows {
		fmt.Fprintf(out, "%-32s %d\n", row.name, row.issue.Count)
		for _, id := range row.issue.Sample {
			fmt.Fprintf(out, "  %s\n", id)
		}
		if row.problem {
			problems += row.issue.Count
		}
	}
	if problems > 0 {
		return fmt.Errorf("%d invariant violations found", problems)
	}
	return nil
}

func retentionRun(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("retention", flag.ContinueOnError)
	execute := fs.Bool("execute", false, "permanently delete the media of due events (default: list only)")
	limit := fs.Int("limit", 25, "events handled in this run")
	grace := fs.Duration("grace", retention.DefaultGrace, "deletion grace period after a storage period ends")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Minute)
	defer cancel()

	opts := []retention.Option{retention.WithGrace(*grace), retention.WithEnforcement(*execute)}
	var objects retention.ObjectStore = noObjects{}
	if *execute {
		store, err := r2.New(ctx, os.Getenv("R2_ENDPOINT"), envOr("R2_REGION", "auto"), os.Getenv("R2_BUCKET"),
			os.Getenv("R2_ACCESS_KEY_ID"), os.Getenv("R2_SECRET_ACCESS_KEY"))
		if err != nil {
			return fmt.Errorf("open storage: %w", err)
		}
		objects = store
		if os.Getenv("GOOGLE_DRIVE_REFRESH_TOKEN") != "" {
			drive, err := googledrive.New(ctx, os.Getenv("GOOGLE_DRIVE_CLIENT_ID"), os.Getenv("GOOGLE_DRIVE_CLIENT_SECRET"),
				os.Getenv("GOOGLE_DRIVE_REFRESH_TOKEN"), os.Getenv("GOOGLE_DRIVE_ROOT_FOLDER_ID"))
			if err != nil {
				return fmt.Errorf("open archive: %w", err)
			}
			opts = append(opts, retention.WithArchive(drive))
		}
	}
	service := retention.NewService(retention.NewGormRepository(d.db), objects, opts...)
	report, err := service.Run(ctx, *limit, !*execute)
	if err != nil {
		return err
	}
	for _, c := range report.Candidates {
		fmt.Fprintf(out, "  %s  %-10s storage ended %s\n", c.EventID, c.PlanCode, c.RetentionExpiresAt.UTC().Format(time.RFC3339))
	}
	if !*execute {
		fmt.Fprintf(out, "due: %d (nothing deleted; re-run with --execute to delete)\n", report.Due)
		return nil
	}
	fmt.Fprintf(out, "due: %d, purged: %d, failed: %d\n", report.Due, report.Purged, len(report.Failed))
	for _, f := range report.Failed {
		fmt.Fprintf(out, "  %s: %v\n", f.EventID, f.Err)
	}
	if len(report.Failed) > 0 {
		return fmt.Errorf("%d events could not be purged; they are retried on the next run", len(report.Failed))
	}
	return nil
}

// noObjects backs a listing run, which never deletes.
type noObjects struct{}

func (noObjects) DeletePrefix(context.Context, string) (int, error) {
	return 0, fmt.Errorf("planctl: storage is not opened for a listing run")
}

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func priceMap(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: planctl price-map <list|set|retire> [flags]")
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()

	ctx := context.Background()

	switch args[0] {
	case "list":
		type row struct {
			ID              uuid.UUID
			Provider        string
			PlanCode        string
			Version         int
			Currency        string
			AmountMinor     int64
			ProviderPriceID string
			Active          bool
			CreatedAt       time.Time
			RetiredAt       *time.Time
		}
		var rows []row
		err := d.db.WithContext(ctx).Raw(`
			SELECT p.id, p.provider, v.code as plan_code, v.version, p.currency, p.amount_minor, p.provider_price_id, p.active, p.created_at, p.retired_at
			FROM billing_provider_prices p
			JOIN plan_versions v ON p.plan_version_id = v.id
			ORDER BY p.provider, v.code, p.currency, p.active DESC, p.created_at DESC
		`).Scan(&rows).Error
		if err != nil {
			return fmt.Errorf("list prices: %w", err)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "no provider price mappings found")
			return nil
		}
		fmt.Fprintf(out, "%-10s %-14s %-5s %-8s %-28s %-8s %s\n", "PROVIDER", "PLAN", "CURR", "AMOUNT", "PROVIDER PRICE ID", "ACTIVE", "RETIRED AT")
		for _, r := range rows {
			retired := "—"
			if r.RetiredAt != nil {
				retired = r.RetiredAt.UTC().Format(time.RFC3339)
			}
			activeStr := "yes"
			if !r.Active {
				activeStr = "no"
			}
			fmt.Fprintf(out, "%-10s %-14s %-5s %-8d %-28s %-8s %s\n", r.Provider, fmt.Sprintf("%s (v%d)", r.PlanCode, r.Version), r.Currency, r.AmountMinor, r.ProviderPriceID, activeStr, retired)
		}
		return nil

	case "set":
		fs := flag.NewFlagSet("price-map set", flag.ContinueOnError)
		providerFlag := fs.String("provider", "paddle", "payment provider (paddle)")
		planFlag := fs.String("plan", "", "plan code (experience|signature)")
		currFlag := fs.String("currency", "USD", "ISO 4217 currency code")
		priceIDFlag := fs.String("price-id", "", "provider price id (e.g. pri_...)")
		amountFlag := fs.Int64("amount-minor", -1, "checkout amount in minor currency units")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *planFlag == "" || *priceIDFlag == "" || *amountFlag < 0 {
			return fmt.Errorf("--plan, --price-id and a non-negative --amount-minor are required")
		}
		planCode := catalog.PlanCode(*planFlag)
		if !planCode.Sellable() {
			return fmt.Errorf("plan must be a sellable plan")
		}
		version, err := d.plans.ActiveVersion(ctx, planCode)
		if err != nil {
			return fmt.Errorf("find active version for %s: %w", planCode, err)
		}
		currency := strings.ToUpper(strings.TrimSpace(*currFlag))
		provider := strings.TrimSpace(*providerFlag)
		priceID := strings.TrimSpace(*priceIDFlag)

		now := time.Now().UTC()
		err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&billing.ProviderPrice{}).
				Where("provider = ? AND plan_version_id = ? AND currency = ? AND amount_minor = ? AND active = true", provider, version.ID, currency, *amountFlag).
				Updates(map[string]any{
					"active":     false,
					"retired_at": now,
				}).Error; err != nil {
				return err
			}
			newPrice := billing.ProviderPrice{
				ID:              uuid.New(),
				Provider:        billing.Provider(provider),
				PlanVersionID:   version.ID,
				Currency:        currency,
				ProviderPriceID: priceID,
				AmountMinor:     *amountFlag,
				Active:          true,
				CreatedAt:       now,
			}
			return tx.Create(&newPrice).Error
		})
		if err != nil {
			return fmt.Errorf("save price mapping: %w", err)
		}
		fmt.Fprintf(out, "configured provider price mapping: provider=%s plan=%s (v%d) currency=%s amount_minor=%d price_id=%s\n", provider, planCode, version.Version, currency, *amountFlag, priceID)
		return nil

	case "retire":
		fs := flag.NewFlagSet("price-map retire", flag.ContinueOnError)
		providerFlag := fs.String("provider", "paddle", "payment provider")
		priceIDFlag := fs.String("price-id", "", "provider price id to retire")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *priceIDFlag == "" {
			return fmt.Errorf("--price-id is required")
		}
		now := time.Now().UTC()
		res := d.db.WithContext(ctx).Model(&billing.ProviderPrice{}).
			Where("provider = ? AND provider_price_id = ? AND active = true", *providerFlag, *priceIDFlag).
			Updates(map[string]any{
				"active":     false,
				"retired_at": now,
			})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			fmt.Fprintln(out, "no active mapping found with that price ID")
			return nil
		}
		fmt.Fprintf(out, "retired provider price mapping: %s\n", *priceIDFlag)
		return nil

	default:
		return fmt.Errorf("unknown price-map action %q (use list, set, or retire)", args[0])
	}
}

func billingReconcile(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("billing-reconcile", flag.ContinueOnError)
	batchFlag := fs.Int("batch", 50, "maximum number of unapplied purchases to reconcile")
	if err := fs.Parse(args); err != nil {
		return err
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()

	ctx := context.Background()
	repo := billing.NewGormRepository(d.db)

	unapplied, err := repo.UnappliedSettled(ctx, *batchFlag)
	if err != nil {
		return fmt.Errorf("query unapplied purchases: %w", err)
	}

	if len(unapplied) == 0 {
		fmt.Fprintln(out, "no unapplied settled purchases found; all billing grants in sync")
		return nil
	}

	fmt.Fprintf(out, "found %d unapplied settled purchases; applying grants...\n", len(unapplied))
	applied := 0
	for _, p := range unapplied {
		settledAt := time.Now().UTC()
		if p.SettledAt != nil {
			settledAt = *p.SettledAt
		}
		_, grantErr := d.grants.Activate(ctx, entitlement.ActivateGrantInput{
			EventID:        p.EventID,
			TargetPlanCode: p.PlanCode,
			Source:         entitlement.SourceExternal,
			SourceRef:      "billing-purchase:" + p.ID.String(),
			Reason:         "paid event plan purchase reconciliation",
			ActivatedAt:    settledAt,
		})
		if grantErr != nil && !errors.Is(grantErr, entitlement.ErrDuplicateSourceReference) {
			fmt.Fprintf(out, "  purchase %s (event %s): error: %v\n", p.ID, p.EventID, grantErr)
			continue
		}
		if err := repo.MarkEntitlementApplied(ctx, p.ID); err != nil {
			fmt.Fprintf(out, "  purchase %s: mark applied error: %v\n", p.ID, err)
			continue
		}
		applied++
		fmt.Fprintf(out, "  purchase %s: applied %s grant to event %s\n", p.ID, p.PlanCode, p.EventID)
	}
	fmt.Fprintf(out, "reconciliation complete: %d of %d applied\n", applied, len(unapplied))
	return nil
}

func derefInt64(v *int64) string {
	if v == nil {
		return "—"
	}
	return fmt.Sprint(*v)
}

func billingShow(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("billing-show", flag.ContinueOnError)
	purchaseFlag := fs.String("id", "", "purchase id")
	eventFlag := fs.String("event", "", "event id")
	txnFlag := fs.String("transaction", "", "provider transaction id (e.g. txn_...)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *purchaseFlag == "" && *eventFlag == "" && *txnFlag == "" {
		return fmt.Errorf("at least one of --id, --event, or --transaction is required")
	}

	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()

	ctx := context.Background()
	query := d.db.WithContext(ctx).Model(&billing.Purchase{})
	if *purchaseFlag != "" {
		pID, err := uuid.Parse(*purchaseFlag)
		if err != nil {
			return fmt.Errorf("--id must be a valid UUID: %w", err)
		}
		query = query.Where("id = ?", pID)
	}
	if *eventFlag != "" {
		eID, err := uuid.Parse(*eventFlag)
		if err != nil {
			return fmt.Errorf("--event must be a valid UUID: %w", err)
		}
		query = query.Where("event_id = ?", eID)
	}
	if *txnFlag != "" {
		query = query.Where("provider_transaction_id = ?", strings.TrimSpace(*txnFlag))
	}

	var purchases []billing.Purchase
	if err := query.Order("created_at DESC").Limit(20).Find(&purchases).Error; err != nil {
		return fmt.Errorf("find purchases: %w", err)
	}

	if len(purchases) == 0 {
		fmt.Fprintln(out, "no matching purchases found")
		return nil
	}

	for _, p := range purchases {
		fmt.Fprintln(out, "────────────────────────────────────────────────────────────")
		fmt.Fprintf(out, "PURCHASE ID:      %s\n", p.ID)
		fmt.Fprintf(out, "Event ID:         %s\n", p.EventID)
		fmt.Fprintf(out, "Host ID:          %s\n", p.HostID)
		fmt.Fprintf(out, "Plan Code:        %s\n", p.PlanCode)
		fmt.Fprintf(out, "Status:           %s\n", p.Status)
		fmt.Fprintf(out, "Quoted:           %d %s\n", p.QuotedAmountMinor, p.QuotedCurrency)
		fmt.Fprintf(out, "Pricing:          base=%d discount=%d upgrade_credit=%d reason=%s context=%s version=%s\n",
			p.BaseAmountMinor, p.DiscountAmountMinor, p.UpgradeCreditMinor,
			p.PricingReason, p.PricingContext, p.PricingVersion)
		if p.RefundedAmountMinor > 0 {
			fmt.Fprintf(out, "Refunded:         %d %s\n", p.RefundedAmountMinor, p.QuotedCurrency)
		}
		if p.SettledTotalMinor != nil && p.SettledCurrency != nil {
			fmt.Fprintf(out, "Settled:          %d %s (subtotal: %s, tax: %s)\n",
				*p.SettledTotalMinor, *p.SettledCurrency, derefInt64(p.SettledSubtotalMinor), derefInt64(p.SettledTaxMinor))
		}
		fmt.Fprintf(out, "Provider:         %s\n", p.Provider)
		fmt.Fprintf(out, "Provider Txn ID:  %s\n", deref(p.ProviderTransactionID))
		fmt.Fprintf(out, "Provider Chk ID:  %s\n", deref(p.ProviderCheckoutID))
		fmt.Fprintf(out, "Provider Price:   %s\n", p.ProviderPriceID)
		var billingStatus string
		if err := d.db.WithContext(ctx).Raw(`SELECT billing_status FROM events WHERE id = ?`, p.EventID).
			Scan(&billingStatus).Error; err == nil && billingStatus != "" && billingStatus != "ok" {
			fmt.Fprintf(out, "Event Billing:    %s (needs a human decision; nothing was revoked)\n", billingStatus)
		}
		fmt.Fprintf(out, "Idempotency Key:  %s\n", p.IdempotencyKey)
		fmt.Fprintf(out, "Created At:       %s\n", p.CreatedAt.UTC().Format(time.RFC3339))
		if p.SettledAt != nil {
			fmt.Fprintf(out, "Settled At:       %s\n", p.SettledAt.UTC().Format(time.RFC3339))
		}
		if p.EntitlementAppliedAt != nil {
			fmt.Fprintf(out, "Entitlement At:   %s\n", p.EntitlementAppliedAt.UTC().Format(time.RFC3339))
		} else if p.Status == billing.StatusSettled {
			fmt.Fprintf(out, "Entitlement At:   NOT APPLIED (run 'planctl billing-reconcile' to fix)\n")
		}

		// List associated provider events
		var events []billing.ProviderEventRecord
		if err := d.db.WithContext(ctx).Where("purchase_id = ?", p.ID).Order("received_at ASC").Find(&events).Error; err == nil && len(events) > 0 {
			fmt.Fprintln(out, "Webhook Events:")
			for _, ev := range events {
				errStr := "none"
				if ev.LastError != nil {
					errStr = *ev.LastError
				}
				fmt.Fprintf(out, "  - %s [%s] ext_id=%s attempts=%d err=%s at=%s\n",
					ev.EventType, ev.Status, ev.ExternalEventID, ev.AttemptCount, errStr, ev.ReceivedAt.UTC().Format(time.RFC3339))
			}
		}

		// Show active plan for the event
		ent, err := d.reader.Resolve(ctx, p.EventID)
		if err == nil {
			fmt.Fprintf(out, "Current Event Plan: %s v%d (source: %s, uploads until: %s)\n",
				ent.PlanCode, ent.PlanVersion, ent.Source, ent.UploadExpiresAt.UTC().Format(time.RFC3339))
		}
	}
	return nil
}

// productMap manages the provider product anchors. One product per sellable
// plan is all the provider catalog needs: every amount the pricing engine
// produces is sent as a non-catalog price under it, so a coupon, an upgrade
// credit or a promotion never requires a new provider price record.
func productMap(args []string, out io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: planctl product-map <list|set|retire> [flags]")
	}
	d, err := open()
	if err != nil {
		return err
	}
	defer d.Close()
	ctx := context.Background()

	switch args[0] {
	case "list":
		var rows []billing.ProviderProduct
		if err := d.db.WithContext(ctx).Order("provider, plan_code, active DESC, created_at DESC").Find(&rows).Error; err != nil {
			return fmt.Errorf("list products: %w", err)
		}
		if len(rows) == 0 {
			fmt.Fprintln(out, "no provider product mappings found")
			return nil
		}
		fmt.Fprintf(out, "%-10s %-14s %-30s %-8s %s\n", "PROVIDER", "PLAN", "PROVIDER PRODUCT ID", "ACTIVE", "RETIRED AT")
		for _, r := range rows {
			retired := "—"
			if r.RetiredAt != nil {
				retired = r.RetiredAt.UTC().Format(time.RFC3339)
			}
			active := "yes"
			if !r.Active {
				active = "no"
			}
			fmt.Fprintf(out, "%-10s %-14s %-30s %-8s %s\n", r.Provider, r.PlanCode, r.ProviderProductID, active, retired)
		}
		return nil

	case "set":
		fs := flag.NewFlagSet("product-map set", flag.ContinueOnError)
		providerFlag := fs.String("provider", "paddle", "payment provider (paddle)")
		planFlag := fs.String("plan", "", "plan code (experience|signature)")
		productFlag := fs.String("product-id", "", "provider product id (e.g. pro_...)")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		planCode := catalog.PlanCode(strings.TrimSpace(*planFlag))
		productID := strings.TrimSpace(*productFlag)
		if productID == "" || !planCode.Sellable() {
			return fmt.Errorf("--plan must be a sellable plan and --product-id is required")
		}
		provider := strings.TrimSpace(*providerFlag)
		now := time.Now().UTC()
		err = d.db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
			if err := tx.Model(&billing.ProviderProduct{}).
				Where("provider = ? AND plan_code = ? AND active = true", provider, planCode).
				Updates(map[string]any{"active": false, "retired_at": now}).Error; err != nil {
				return err
			}
			return tx.Create(&billing.ProviderProduct{
				ID: uuid.New(), Provider: billing.Provider(provider), PlanCode: planCode,
				ProviderProductID: productID, Active: true, CreatedAt: now,
			}).Error
		})
		if err != nil {
			return fmt.Errorf("save product mapping: %w", err)
		}
		fmt.Fprintf(out, "configured provider product: provider=%s plan=%s product_id=%s\n", provider, planCode, productID)
		return nil

	case "retire":
		fs := flag.NewFlagSet("product-map retire", flag.ContinueOnError)
		providerFlag := fs.String("provider", "paddle", "payment provider")
		planFlag := fs.String("plan", "", "plan code to retire")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *planFlag == "" {
			return fmt.Errorf("--plan is required")
		}
		now := time.Now().UTC()
		res := d.db.WithContext(ctx).Model(&billing.ProviderProduct{}).
			Where("provider = ? AND plan_code = ? AND active = true", *providerFlag, *planFlag).
			Updates(map[string]any{"active": false, "retired_at": now})
		if res.Error != nil {
			return res.Error
		}
		if res.RowsAffected == 0 {
			return fmt.Errorf("no active product mapping for %s/%s", *providerFlag, *planFlag)
		}
		fmt.Fprintf(out, "retired provider product: provider=%s plan=%s\n", *providerFlag, *planFlag)
		return nil

	default:
		return fmt.Errorf("usage: planctl product-map <list|set|retire> [flags]")
	}
}
