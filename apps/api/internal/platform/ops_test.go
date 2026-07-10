package platform_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/platform"
	"sdano.app/api/internal/testdb"
)

// lastAudit returns the most recently inserted ops_audit row, so tests can
// assert on action/tenant_id/detail without a dedicated sqlc query.
func lastAudit(t *testing.T, pool *pgxpool.Pool) db.OpsAudit {
	t.Helper()
	row := pool.QueryRow(context.Background(),
		`SELECT id, action, tenant_id, detail, performed_at FROM ops_audit ORDER BY performed_at DESC, id DESC LIMIT 1`)
	var a db.OpsAudit
	if err := row.Scan(&a.ID, &a.Action, &a.TenantID, &a.Detail, &a.PerformedAt); err != nil {
		t.Fatalf("querying last audit row: %v", err)
	}
	return a
}

func TestOpsCreateTenant(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	before := time.Now()
	result, err := platform.OpsCreateTenant(ctx, pool, "Acme Cleaning", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}
	after := time.Now()

	if result.TenantID == uuid.Nil {
		t.Fatal("expected non-nil tenant id")
	}
	if result.AdminEmail == "" {
		t.Fatal("expected non-empty admin email")
	}
	if len(result.AdminPassword) != 24 {
		t.Fatalf("expected 24-char password, got %d chars", len(result.AdminPassword))
	}

	// Tenant row: status trial, trial_ends_at ~= now + 30d.
	var status db.TenantStatus
	var trialEndsAt time.Time
	if err := pool.QueryRow(ctx, `SELECT status, trial_ends_at FROM tenant WHERE id = $1`, result.TenantID).
		Scan(&status, &trialEndsAt); err != nil {
		t.Fatalf("querying tenant: %v", err)
	}
	if status != db.TenantStatusTrial {
		t.Errorf("expected status trial, got %s", status)
	}
	wantFrom := before.Add(30 * 24 * time.Hour).Add(-time.Minute)
	wantTo := after.Add(30 * 24 * time.Hour).Add(time.Minute)
	if trialEndsAt.Before(wantFrom) || trialEndsAt.After(wantTo) {
		t.Errorf("trial_ends_at %v not within a minute of now+30d (want between %v and %v)", trialEndsAt, wantFrom, wantTo)
	}

	// Admin user: role admin, active, verifiable password.
	var role db.UserRole
	var isActive bool
	var email *string
	var passwordHash *string
	if err := pool.QueryRow(ctx,
		`SELECT role, is_active, email, password_hash FROM app_user WHERE tenant_id = $1`, result.TenantID).
		Scan(&role, &isActive, &email, &passwordHash); err != nil {
		t.Fatalf("querying admin user: %v", err)
	}
	if role != db.UserRoleAdmin {
		t.Errorf("expected role admin, got %s", role)
	}
	if !isActive {
		t.Error("expected admin user to be active")
	}
	if email == nil || *email != result.AdminEmail {
		t.Errorf("expected email %q, got %v", result.AdminEmail, email)
	}
	if passwordHash == nil {
		t.Fatal("expected a password hash")
	}
	ok, err := auth.VerifyPassword(result.AdminPassword, *passwordHash)
	if err != nil {
		t.Fatalf("VerifyPassword: %v", err)
	}
	if !ok {
		t.Error("generated password must verify against the stored hash")
	}

	// Audit row: tenant.create, valid JSON detail.
	a := lastAudit(t, pool)
	if a.Action != "tenant.create" {
		t.Errorf("expected action tenant.create, got %s", a.Action)
	}
	if !a.TenantID.Valid || a.TenantID.UUID != result.TenantID {
		t.Errorf("expected audit tenant_id %s, got %v", result.TenantID, a.TenantID)
	}
	var detail map[string]any
	if err := json.Unmarshal(a.Detail, &detail); err != nil {
		t.Errorf("audit detail must be valid JSON: %v", err)
	}
}

// TestOpsCreateTenantAdminEmailIsAsciiSlug covers the slug fallback: a
// tenant name with no [a-z0-9] characters (e.g. non-latin) must still
// produce a valid, ASCII admin email, derived from the tenant id.
func TestOpsCreateTenantAdminEmailIsAsciiSlug(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	result, err := platform.OpsCreateTenant(ctx, pool, "ЧистоГрад", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}
	if !strings.HasSuffix(result.AdminEmail, ".sdano.local") || !strings.HasPrefix(result.AdminEmail, "admin@") {
		t.Fatalf("expected admin@<slug>.sdano.local shape, got %q", result.AdminEmail)
	}
	local := strings.TrimSuffix(strings.TrimPrefix(result.AdminEmail, "admin@"), ".sdano.local")
	for _, r := range local {
		if (r < 'a' || r > 'z') && (r < '0' || r > '9') {
			t.Fatalf("slug %q must be [a-z0-9] only, found %q", local, r)
		}
	}

	// A second tenant with the exact same non-latin name must not collide:
	// the fallback keys off the (unique) tenant id, not the name.
	result2, err := platform.OpsCreateTenant(ctx, pool, "ЧистоГрад", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant (second, same name): %v", err)
	}
	if result2.AdminEmail == result.AdminEmail {
		t.Fatalf("two tenants with the same non-latin name must not collide on admin email, got %q twice", result.AdminEmail)
	}
}

// TestOpsCreateTenantAdminEmailCollisionFails covers the self-review
// concern: a slug collision (two tenants whose latin-derived slugs match)
// must surface as a clear error, not a silent retry or partial tenant.
func TestOpsCreateTenantAdminEmailCollisionFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := platform.OpsCreateTenant(ctx, pool, "Acme", 30); err != nil {
		t.Fatalf("first OpsCreateTenant: %v", err)
	}

	_, err := platform.OpsCreateTenant(ctx, pool, "Acme", 30)
	if err == nil {
		t.Fatal("expected an error on admin email collision, got nil")
	}

	rows, err := platform.OpsListTenants(ctx, pool)
	if err != nil {
		t.Fatalf("OpsListTenants: %v", err)
	}
	count := 0
	for _, r := range rows {
		if r.Name == "Acme" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected exactly 1 committed 'Acme' tenant after the failed second create (all-or-nothing tx), got %d", count)
	}
}

func TestOpsCreateTenantRejectsEmptyName(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := platform.OpsCreateTenant(ctx, pool, "   ", 30); err == nil {
		t.Fatal("expected an error for a blank tenant name")
	}
}

func TestOpsListTenants(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if _, err := platform.OpsCreateTenant(ctx, pool, "Beta Corp", 14); err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}

	rows, err := platform.OpsListTenants(ctx, pool)
	if err != nil {
		t.Fatalf("OpsListTenants: %v", err)
	}
	found := false
	for _, r := range rows {
		if r.Name == "Beta Corp" {
			found = true
			if r.Status != db.TenantStatusTrial {
				t.Errorf("expected trial status, got %s", r.Status)
			}
		}
	}
	if !found {
		t.Fatal("expected the created tenant to appear in OpsListTenants")
	}
}

func TestOpsSuspendAndActivate(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	result, err := platform.OpsCreateTenant(ctx, pool, "Gamma LLC", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}

	if err := platform.OpsSuspend(ctx, pool, result.TenantID, "non-payment"); err != nil {
		t.Fatalf("OpsSuspend: %v", err)
	}

	var status db.TenantStatus
	var suspendedAt *time.Time
	if err := pool.QueryRow(ctx, `SELECT status, suspended_at FROM tenant WHERE id = $1`, result.TenantID).
		Scan(&status, &suspendedAt); err != nil {
		t.Fatalf("querying tenant: %v", err)
	}
	if status != db.TenantStatusSuspended {
		t.Errorf("expected status suspended, got %s", status)
	}
	if suspendedAt == nil {
		t.Error("expected suspended_at to be set")
	}

	a := lastAudit(t, pool)
	if a.Action != "tenant.suspend" {
		t.Errorf("expected action tenant.suspend, got %s", a.Action)
	}
	var detail map[string]any
	if err := json.Unmarshal(a.Detail, &detail); err != nil {
		t.Errorf("audit detail must be valid JSON: %v", err)
	}
	if detail["note"] != "non-payment" {
		t.Errorf("expected note in audit detail, got %v", detail)
	}

	if err := platform.OpsActivate(ctx, pool, result.TenantID); err != nil {
		t.Fatalf("OpsActivate: %v", err)
	}
	if err := pool.QueryRow(ctx, `SELECT status, suspended_at FROM tenant WHERE id = $1`, result.TenantID).
		Scan(&status, &suspendedAt); err != nil {
		t.Fatalf("querying tenant: %v", err)
	}
	if status != db.TenantStatusActive {
		t.Errorf("expected status active, got %s", status)
	}
	if suspendedAt != nil {
		t.Errorf("expected suspended_at to be cleared, got %v", suspendedAt)
	}

	a = lastAudit(t, pool)
	if a.Action != "tenant.activate" {
		t.Errorf("expected action tenant.activate, got %s", a.Action)
	}
	if err := json.Unmarshal(a.Detail, &detail); err != nil {
		t.Errorf("audit detail must be valid JSON: %v", err)
	}
}

func TestOpsSuspendUnknownTenantFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if err := platform.OpsSuspend(ctx, pool, uuid.New(), "note"); err == nil {
		t.Fatal("expected an error suspending a nonexistent tenant")
	}
}

func TestOpsActivateUnknownTenantFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if err := platform.OpsActivate(ctx, pool, uuid.New()); err == nil {
		t.Fatal("expected an error activating a nonexistent tenant")
	}
}

func TestOpsSetBilling(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	result, err := platform.OpsCreateTenant(ctx, pool, "Delta Inc", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}

	billedUntil := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	if err := platform.OpsSetBilling(ctx, pool, result.TenantID, billedUntil, "50$/mo, 20 objects"); err != nil {
		t.Fatalf("OpsSetBilling: %v", err)
	}

	var gotBilledUntil time.Time
	var planNote *string
	if err := pool.QueryRow(ctx, `SELECT billed_until, plan_note FROM tenant WHERE id = $1`, result.TenantID).
		Scan(&gotBilledUntil, &planNote); err != nil {
		t.Fatalf("querying tenant: %v", err)
	}
	if !gotBilledUntil.Equal(billedUntil) {
		t.Errorf("expected billed_until %v, got %v", billedUntil, gotBilledUntil)
	}
	if planNote == nil || *planNote != "50$/mo, 20 objects" {
		t.Errorf("expected plan_note to be set, got %v", planNote)
	}

	a := lastAudit(t, pool)
	if a.Action != "tenant.set-billing" {
		t.Errorf("expected action tenant.set-billing, got %s", a.Action)
	}
	var detail map[string]any
	if err := json.Unmarshal(a.Detail, &detail); err != nil {
		t.Errorf("audit detail must be valid JSON: %v", err)
	}
	if detail["billed_until"] != "2026-09-01" {
		t.Errorf("expected billed_until in audit detail, got %v", detail)
	}
	if detail["plan_note"] != "50$/mo, 20 objects" {
		t.Errorf("expected plan_note in audit detail, got %v", detail)
	}
}

// TestOpsSetBillingEmptyPlanNoteKeepsExisting exercises the COALESCE
// semantics of OpsSetBillingParams.PlanNote: an empty --plan-note must not
// clobber a previously recorded note.
func TestOpsSetBillingEmptyPlanNoteKeepsExisting(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	result, err := platform.OpsCreateTenant(ctx, pool, "Epsilon", 30)
	if err != nil {
		t.Fatalf("OpsCreateTenant: %v", err)
	}
	if err := platform.OpsSetBilling(ctx, pool, result.TenantID, time.Now(), "original note"); err != nil {
		t.Fatalf("OpsSetBilling (first): %v", err)
	}
	if err := platform.OpsSetBilling(ctx, pool, result.TenantID, time.Now().AddDate(0, 1, 0), ""); err != nil {
		t.Fatalf("OpsSetBilling (second, empty note): %v", err)
	}

	var planNote *string
	if err := pool.QueryRow(ctx, `SELECT plan_note FROM tenant WHERE id = $1`, result.TenantID).Scan(&planNote); err != nil {
		t.Fatalf("querying tenant: %v", err)
	}
	if planNote == nil || *planNote != "original note" {
		t.Errorf("expected plan_note to remain 'original note', got %v", planNote)
	}
}

func TestOpsSetBillingUnknownTenantFails(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	if err := platform.OpsSetBilling(ctx, pool, uuid.New(), time.Now(), "note"); err == nil {
		t.Fatal("expected an error setting billing on a nonexistent tenant")
	}
}

// TestOpsErrTenantNotFoundIsPgxNoRows guards the sentinel used to
// distinguish "tenant does not exist" from other DB errors, so cmd/ops can
// print a clear message instead of a raw driver error.
func TestOpsErrTenantNotFoundIsPgxNoRows(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	err := platform.OpsSuspend(ctx, pool, uuid.New(), "")
	if !errors.Is(err, platform.ErrTenantNotFound) {
		t.Fatalf("expected errors.Is(err, platform.ErrTenantNotFound), got %v", err)
	}
	if errors.Is(err, pgx.ErrNoRows) {
		t.Fatal("ErrTenantNotFound must be its own sentinel, not a leaked pgx.ErrNoRows")
	}
}
