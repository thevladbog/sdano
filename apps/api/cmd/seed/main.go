// Command seed is a dev-only tool that populates a demo tenant
// («Демо — ЧистоГрад») with a contract, ten objects (from db/seed/objects.csv),
// a versioned cleaning checklist, two workers with invite codes, and a
// week of pre-generated work orders — everything `make dev-up && make
// migrate-up && make seed-demo` needs for a local walkthrough or the
// live end-to-end report smoke test. Never runs in production; not part
// of the api binary.
//
// Idempotent by refusal, not by upsert: a second run finds the demo
// tenant already exists and aborts (exit 1) rather than creating a
// duplicate tenant with colliding "DEMO-N" qr_tokens (qr_token is
// globally unique) or a second confusing "Демо — ЧистоГрад" row.
package main

import (
	"context"
	"encoding/csv"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
	"sdano.app/api/internal/platform"
	"sdano.app/api/internal/roster"
)

const (
	demoTenantName     = "Демо — ЧистоГрад"
	demoTenantTZ       = "Europe/Moscow"
	demoTrialDays      = 30
	demoContractName   = "Контракт с администрацией города"
	demoContractClient = "Администрация города Екатеринбурга"
	demoTemplateName   = "Уборка остановки"
	demoWorker1Name    = "Алексей, бригада 1"
	demoWorker2Name    = "Сергей, бригада 2"
	demoOrderDays      = 7 // a full week of pre-generated orders, starting today
	demoObjectKind     = "bus_stop"
)

// demoChecklistItems are the four checklist steps, in display order. Only
// the last ("photo after cleaning") requires photo evidence — mirrors the
// realistic minimum a municipal client actually disputes: proof the job
// was done, not a photo of every trash bag.
var demoChecklistItems = []struct {
	title         string
	requiresPhoto bool
}{
	{"Собрать мусор", false},
	{"Вымыть павильон", false},
	{"Удалить граффити/объявления", false},
	{"Фото после уборки", true},
}

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "seed:", err)
		os.Exit(1)
	}
}

func run() error {
	dbURL := os.Getenv("DATABASE_URL")
	if dbURL == "" {
		return errors.New("DATABASE_URL is required")
	}

	ctx := context.Background()
	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		return fmt.Errorf("connecting to database: %w", err)
	}
	defer pool.Close()
	if err := pool.Ping(ctx); err != nil {
		return fmt.Errorf("pinging database: %w", err)
	}

	q := db.New(pool)

	// Idempotence guard, before any write: a prior successful run left the
	// demo tenant in place. Abort loudly with a friendly message instead of
	// creating a duplicate — see the package doc for why an upsert isn't
	// the right shape here.
	if _, err := q.GetTenantByName(ctx, demoTenantName); err == nil {
		return fmt.Errorf("demo tenant %q already exists — seed-demo has already run; nothing to do", demoTenantName)
	} else if !errors.Is(err, pgx.ErrNoRows) {
		return fmt.Errorf("checking for an existing demo tenant: %w", err)
	}

	rows, err := loadObjectRows()
	if err != nil {
		return err
	}

	// OpsCreateTenant commits its own transaction (tenant + admin + audit —
	// see its doc comment in internal/platform/ops.go). Everything else
	// below runs in one second transaction; the idempotence guard above is
	// what makes a rerun safe, not transactional atomicity across the two.
	tenant, err := platform.OpsCreateTenant(ctx, pool, demoTenantName, demoTrialDays)
	if err != nil {
		return fmt.Errorf("creating demo tenant: %w", err)
	}
	if err := q.SetTenantTimezone(ctx, db.SetTenantTimezoneParams{ID: tenant.TenantID, Timezone: demoTenantTZ}); err != nil {
		return fmt.Errorf("setting demo tenant timezone: %w", err)
	}

	loc, err := time.LoadLocation(demoTenantTZ)
	if err != nil {
		return fmt.Errorf("loading %s location: %w", demoTenantTZ, err)
	}
	now := time.Now().In(loc)
	// Tenant-local "today" as a date-only value in UTC, matching how
	// workorder.TenantToday computes it elsewhere in the codebase — pgtype.Date
	// only carries year/month/day, so the zone of the Time value itself must
	// not shift the calendar date.
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := q.WithTx(tx)

	clientName := demoContractClient
	contract, err := qtx.InsertContract(ctx, db.InsertContractParams{
		TenantID: tenant.TenantID, Name: demoContractName, ClientName: &clientName,
	})
	if err != nil {
		return fmt.Errorf("inserting demo contract: %w", err)
	}

	objectIDs := make([]uuid.UUID, 0, len(rows))
	for i, r := range rows {
		qrToken := fmt.Sprintf("DEMO-%d", i+1)
		address, lat, lon := r.address, r.lat, r.lon
		obj, err := qtx.InsertObject(ctx, db.InsertObjectParams{
			TenantID: tenant.TenantID, Name: r.name, Address: &address,
			Lat: &lat, Lon: &lon, Kind: strPtr(demoObjectKind),
			QrToken: &qrToken, ContractID: uuid.NullUUID{UUID: contract.ID, Valid: true},
		})
		if err != nil {
			return fmt.Errorf("inserting demo object %q: %w", r.name, err)
		}
		objectIDs = append(objectIDs, obj.ID)
	}

	template, err := qtx.InsertChecklistTemplate(ctx, db.InsertChecklistTemplateParams{
		TenantID: tenant.TenantID, Name: demoTemplateName,
	})
	if err != nil {
		return fmt.Errorf("inserting demo checklist template: %w", err)
	}
	version, err := qtx.InsertChecklistTemplateVersion(ctx, db.InsertChecklistTemplateVersionParams{
		TemplateID: template.ID, Version: 1,
	})
	if err != nil {
		return fmt.Errorf("inserting demo checklist template version: %w", err)
	}
	for i, item := range demoChecklistItems {
		if _, err := qtx.InsertChecklistTemplateItem(ctx, db.InsertChecklistTemplateItemParams{
			VersionID: version.ID, Position: int32(i + 1), Title: item.title, RequiresPhoto: item.requiresPhoto,
		}); err != nil {
			return fmt.Errorf("inserting demo checklist item %q: %w", item.title, err)
		}
	}

	worker1, err := qtx.InsertWorker(ctx, db.InsertWorkerParams{TenantID: tenant.TenantID, DisplayName: demoWorker1Name})
	if err != nil {
		return fmt.Errorf("inserting demo worker %q: %w", demoWorker1Name, err)
	}
	worker2, err := qtx.InsertWorker(ctx, db.InsertWorkerParams{TenantID: tenant.TenantID, DisplayName: demoWorker2Name})
	if err != nil {
		return fmt.Errorf("inserting demo worker %q: %w", demoWorker2Name, err)
	}

	code1, expires1, err := roster.CreateInvite(ctx, qtx, tenant.TenantID, worker1.ID)
	if err != nil {
		return fmt.Errorf("creating invite for %q: %w", demoWorker1Name, err)
	}
	code2, expires2, err := roster.CreateInvite(ctx, qtx, tenant.TenantID, worker2.ID)
	if err != nil {
		return fmt.Errorf("creating invite for %q: %w", demoWorker2Name, err)
	}

	// A week of pre-generated orders (docs/06 decision 8: no recurrence
	// engine, orders are pre-generated), alternating assignees across the
	// whole 70-order batch so both workers get a realistic, even split.
	assignees := [2]uuid.UUID{worker1.ID, worker2.ID}
	orderCount := 0
	for d := 0; d < demoOrderDays; d++ {
		due := pgtype.Date{Time: today.AddDate(0, 0, d), Valid: true}
		for _, objectID := range objectIDs {
			assignee := assignees[orderCount%2]
			if err := qtx.InsertWorkOrder(ctx, db.InsertWorkOrderParams{
				ID: uuid.New(), TenantID: tenant.TenantID, ObjectID: objectID,
				VersionID: version.ID, AssigneeID: uuid.NullUUID{UUID: assignee, Valid: true},
				DueDate: due,
			}); err != nil {
				return fmt.Errorf("inserting demo work order: %w", err)
			}
			orderCount++
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}

	printSummary(tenant, len(objectIDs), orderCount, code1, expires1, code2, expires2)
	return nil
}

// printSummary is the ONLY place the demo admin password and invite codes
// are ever written anywhere — stdout, once, on a successful seed. Nothing
// in this file logs them via slog or persists them to a file (AGENTS.md:
// "Never log photo URLs with credentials, tokens, or invite codes").
func printSummary(tenant platform.CreateTenantResult, objectCount, orderCount int, code1 string, expires1 time.Time, code2 string, expires2 time.Time) {
	fmt.Println("Demo tenant seeded:", demoTenantName)
	fmt.Println()
	fmt.Println("  Tenant ID:      ", tenant.TenantID)
	fmt.Println("  Admin email:    ", tenant.AdminEmail)
	fmt.Println("  Admin password: ", tenant.AdminPassword)
	fmt.Println()
	fmt.Println("  Worker:", demoWorker1Name)
	fmt.Println("    Invite code:  ", code1, "(expires", expires1.Format(time.RFC3339)+")")
	fmt.Println("  Worker:", demoWorker2Name)
	fmt.Println("    Invite code:  ", code2, "(expires", expires2.Format(time.RFC3339)+")")
	fmt.Println()
	fmt.Println("  Objects:       ", objectCount)
	fmt.Println("  Work orders:   ", orderCount, fmt.Sprintf("(%d days x %d objects)", demoOrderDays, objectCount))
	fmt.Println()
	fmt.Println("These credentials are shown once and are not stored in plaintext anywhere. Save them now.")
}

type objectRow struct {
	name    string
	address string
	lat     float64
	lon     float64
}

// loadObjectRows reads db/seed/objects.csv, resolved relative to this
// source file's location (not the process's working directory) so `go run
// ./cmd/seed` works the same whether invoked from the repo root or
// apps/api — the same trick internal/testdb uses to find db/migrations.
func loadObjectRows() ([]objectRow, error) {
	path, err := objectsCSVPath()
	if err != nil {
		return nil, err
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	r := csv.NewReader(f)
	r.FieldsPerRecord = 4
	records, err := r.ReadAll()
	if err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}

	rows := make([]objectRow, 0, len(records))
	for i, rec := range records {
		lat, err := strconv.ParseFloat(rec[2], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: row %d: invalid lat %q: %w", path, i+1, rec[2], err)
		}
		lon, err := strconv.ParseFloat(rec[3], 64)
		if err != nil {
			return nil, fmt.Errorf("%s: row %d: invalid lon %q: %w", path, i+1, rec[3], err)
		}
		rows = append(rows, objectRow{name: rec[0], address: rec[1], lat: lat, lon: lon})
	}
	return rows, nil
}

func objectsCSVPath() (string, error) {
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		return "", errors.New("resolving this source file's path")
	}
	// self = <root>/apps/api/cmd/seed/main.go
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "..", "db", "seed", "objects.csv"), nil
}

func strPtr(s string) *string { return &s }
