package platform

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

// ErrTenantNotFound is returned by every mutator below when tenantID does
// not name an existing tenant. Without this check a typo'd id would UPDATE
// zero rows yet still write an ops_audit row claiming the action happened —
// a phantom audit entry for a mutation that touched nothing. Detected via
// GetTenantSuspension (db/queries/platform.sql), a plain by-id SELECT that
// doubles as a cheap existence probe.
var ErrTenantNotFound = errors.New("tenant not found")

// defaultPasswordLen is the length of a freshly generated admin password:
// 24 chars over a 62-character alphabet is >142 bits of entropy.
const defaultPasswordLen = 24

const passwordAlphabet = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789"

// slugDisallowed matches every rune that is NOT [a-z0-9], for deriving an
// admin login email's local part from a tenant name.
var slugDisallowed = regexp.MustCompile(`[^a-z0-9]+`)

// CreateTenantResult is OpsCreateTenant's return value: the new tenant's id
// and its first admin's login credentials.
//
// AdminPassword is the ONLY place the plaintext password exists outside the
// operator's terminal: it is never logged (slog or otherwise), never
// persisted anywhere (only the argon2id hash is stored), and never written
// to a file. cmd/ops prints it to stdout exactly once; callers of this
// function must not retain, log, or forward it beyond that single use.
type CreateTenantResult struct {
	TenantID      uuid.UUID
	AdminEmail    string
	AdminPassword string
}

// OpsCreateTenant creates a new tenant (status trial, trial_ends_at = now +
// trialDays) and its first admin user — admin@<slug>.sdano.local, where
// slug is the lowercased [a-z0-9] subset of name, falling back to the first
// 8 hex characters of the new tenant's id when name has no latin/digit
// characters at all (e.g. "ЧистоГрад"). The admin's password is 24
// crypto-random characters, hashed via auth.HashPassword before storage —
// see CreateTenantResult's doc for where the plaintext may travel.
//
// The tenant insert, admin insert, and ops_audit insert ("tenant.create")
// all run in one transaction: a half-created tenant (no admin, or an admin
// nobody can log in as) must never be observable by any other reader.
func OpsCreateTenant(ctx context.Context, pool *pgxpool.Pool, name string, trialDays int) (CreateTenantResult, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		return CreateTenantResult{}, errors.New("tenant name must not be empty")
	}
	if trialDays < 0 {
		return CreateTenantResult{}, fmt.Errorf("trial days must be >= 0, got %d", trialDays)
	}

	password, err := generatePassword()
	if err != nil {
		return CreateTenantResult{}, fmt.Errorf("generating admin password: %w", err)
	}
	hash, err := auth.HashPassword(password)
	if err != nil {
		return CreateTenantResult{}, fmt.Errorf("hashing admin password: %w", err)
	}

	trialEndsAt := pgtype.Timestamptz{
		Time:  time.Now().Add(time.Duration(trialDays) * 24 * time.Hour),
		Valid: true,
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return CreateTenantResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := db.New(pool).WithTx(tx)

	tenantRow, err := qtx.OpsCreateTenant(ctx, db.OpsCreateTenantParams{Name: name, TrialEndsAt: trialEndsAt})
	if err != nil {
		return CreateTenantResult{}, fmt.Errorf("inserting tenant: %w", err)
	}

	slug := slugify(name, tenantRow.ID)
	email := fmt.Sprintf("admin@%s.sdano.local", slug)

	if _, err := qtx.OpsInsertAdminUser(ctx, db.OpsInsertAdminUserParams{
		TenantID:     tenantRow.ID,
		DisplayName:  "Admin",
		Email:        &email,
		PasswordHash: &hash,
	}); err != nil {
		if isUniqueViolation(err) {
			return CreateTenantResult{}, fmt.Errorf(
				"admin email %s is already in use (slug collision for tenant name %q): %w", email, name, err)
		}
		return CreateTenantResult{}, fmt.Errorf("inserting admin user: %w", err)
	}

	detail, err := json.Marshal(map[string]string{"name": name, "admin_email": email})
	if err != nil {
		return CreateTenantResult{}, fmt.Errorf("marshaling audit detail: %w", err)
	}
	if err := qtx.InsertOpsAudit(ctx, db.InsertOpsAuditParams{
		Action:   "tenant.create",
		TenantID: uuid.NullUUID{UUID: tenantRow.ID, Valid: true},
		Detail:   detail,
	}); err != nil {
		return CreateTenantResult{}, fmt.Errorf("inserting audit row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return CreateTenantResult{}, fmt.Errorf("commit: %w", err)
	}

	return CreateTenantResult{TenantID: tenantRow.ID, AdminEmail: email, AdminPassword: password}, nil
}

// OpsListTenants is a pass-through to db.Queries.OpsListTenants: every
// tenant with its worker/object activity counts, for `sdano-ops tenant
// list`.
func OpsListTenants(ctx context.Context, pool *pgxpool.Pool) ([]db.OpsListTenantsRow, error) {
	rows, err := db.New(pool).OpsListTenants(ctx)
	if err != nil {
		return nil, fmt.Errorf("listing tenants: %w", err)
	}
	return rows, nil
}

// OpsSuspend sets tenant.status = 'suspended' and tenant.suspended_at =
// now(), then audits `tenant.suspend` with {"note": note}.
//
// Invariant (depended on by task 8's suspension-enforcement middleware):
// suspended_at is set ONLY here, and cleared ONLY by OpsActivate. Therefore
// `suspended_at IS NOT NULL` and `status = 'suspended'` always agree — any
// future reader may treat either field as authoritative for "is this tenant
// currently suspended" without cross-checking the other.
func OpsSuspend(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, note string) error {
	detail, err := json.Marshal(map[string]string{"note": note})
	if err != nil {
		return fmt.Errorf("marshaling audit detail: %w", err)
	}
	suspendedAt := pgtype.Timestamptz{Time: time.Now(), Valid: true}
	return opsSetStatus(ctx, pool, tenantID, db.TenantStatusSuspended, suspendedAt, "tenant.suspend", detail)
}

// OpsActivate sets tenant.status = 'active' and clears tenant.suspended_at
// (NULL), then audits `tenant.activate`. See OpsSuspend's doc for the
// suspended_at invariant this half establishes.
func OpsActivate(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID) error {
	detail, err := json.Marshal(map[string]string{})
	if err != nil {
		return fmt.Errorf("marshaling audit detail: %w", err)
	}
	// Zero-value pgtype.Timestamptz{} has Valid=false, which pgx encodes as
	// SQL NULL — OpsSetTenantStatus assigns suspended_at directly (no
	// COALESCE), so this clears it.
	return opsSetStatus(ctx, pool, tenantID, db.TenantStatusActive, pgtype.Timestamptz{}, "tenant.activate", detail)
}

// opsSetStatus is the shared transaction body for OpsSuspend/OpsActivate:
// verify the tenant exists, update its status + suspended_at, write the
// audit row, commit. One transaction so a status flip is never observable
// without its audit trail (or vice versa).
func opsSetStatus(
	ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID,
	status db.TenantStatus, suspendedAt pgtype.Timestamptz, action string, detail []byte,
) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := db.New(pool).WithTx(tx)

	if err := assertTenantExists(ctx, qtx, tenantID); err != nil {
		return err
	}

	if err := qtx.OpsSetTenantStatus(ctx, db.OpsSetTenantStatusParams{
		ID: tenantID, Status: status, SuspendedAt: suspendedAt,
	}); err != nil {
		return fmt.Errorf("updating tenant status: %w", err)
	}
	if err := qtx.InsertOpsAudit(ctx, db.InsertOpsAuditParams{
		Action:   action,
		TenantID: uuid.NullUUID{UUID: tenantID, Valid: true},
		Detail:   detail,
	}); err != nil {
		return fmt.Errorf("inserting audit row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// OpsSetBilling sets tenant.billed_until and, when planNote is non-empty,
// tenant.plan_note (an empty planNote leaves the existing note untouched —
// OpsSetBilling's underlying query COALESCEs), then audits
// `tenant.set-billing` with {"billed_until", "plan_note"}.
func OpsSetBilling(ctx context.Context, pool *pgxpool.Pool, tenantID uuid.UUID, billedUntil time.Time, planNote string) error {
	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := db.New(pool).WithTx(tx)

	if err := assertTenantExists(ctx, qtx, tenantID); err != nil {
		return err
	}

	var planNotePtr *string
	if planNote != "" {
		planNotePtr = &planNote
	}

	if err := qtx.OpsSetBilling(ctx, db.OpsSetBillingParams{
		ID:          tenantID,
		BilledUntil: pgtype.Date{Time: billedUntil, Valid: true},
		PlanNote:    planNotePtr,
	}); err != nil {
		return fmt.Errorf("updating billing: %w", err)
	}

	detail, err := json.Marshal(map[string]string{
		"billed_until": billedUntil.Format("2006-01-02"),
		"plan_note":    planNote,
	})
	if err != nil {
		return fmt.Errorf("marshaling audit detail: %w", err)
	}
	if err := qtx.InsertOpsAudit(ctx, db.InsertOpsAuditParams{
		Action:   "tenant.set-billing",
		TenantID: uuid.NullUUID{UUID: tenantID, Valid: true},
		Detail:   detail,
	}); err != nil {
		return fmt.Errorf("inserting audit row: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// assertTenantExists fails loudly with ErrTenantNotFound rather than
// letting a mutator silently no-op (0 rows updated) while still writing an
// audit row for an action that touched nothing.
func assertTenantExists(ctx context.Context, qtx *db.Queries, tenantID uuid.UUID) error {
	if _, err := qtx.GetTenantSuspension(ctx, tenantID); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrTenantNotFound
		}
		return fmt.Errorf("looking up tenant: %w", err)
	}
	return nil
}

// isUniqueViolation reports whether err is a Postgres unique_violation
// (SQLSTATE 23505) — used to turn an admin-email slug collision into a
// clear, non-retried error instead of a generic driver error.
func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

// slugify keeps the [a-z0-9] subset of the lowercased name. If that yields
// nothing (a name with no latin letters or digits at all, e.g.
// "ЧистоГрад"), it falls back to the first 8 hex characters of tenantID —
// unique per tenant, so same-named non-latin tenants never collide on the
// fallback either.
func slugify(name string, tenantID uuid.UUID) string {
	s := slugDisallowed.ReplaceAllString(strings.ToLower(name), "")
	if s != "" {
		return s
	}
	return strings.ReplaceAll(tenantID.String(), "-", "")[:8]
}

// generatePassword returns defaultPasswordLen characters drawn uniformly
// from passwordAlphabet using crypto/rand (via math/big.Int, which performs
// its own rejection sampling internally — no manual modulo-bias handling
// needed here).
func generatePassword() (string, error) {
	alphabetLen := big.NewInt(int64(len(passwordAlphabet)))
	out := make([]byte, defaultPasswordLen)
	for i := range out {
		n, err := rand.Int(rand.Reader, alphabetLen)
		if err != nil {
			return "", fmt.Errorf("reading random bytes: %w", err)
		}
		out[i] = passwordAlphabet[n.Int64()]
	}
	return string(out), nil
}
