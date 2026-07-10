package platform

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/testdb"
)

// fakeStore is a minimal photo.ObjectStore test double. The scheduler only
// ever calls Exists (to decide whether an orphan photo row is safe to
// delete); the remaining methods exist purely to satisfy the interface, like
// report package's fakeStore. errOn lets a test make Exists fail for
// specific keys only, so one erroring photo can sit alongside healthy ones
// in the same GC batch.
type fakeStore struct {
	present map[string]bool
	errOn   map[string]error
}

func newFakeStore(presentKeys ...string) *fakeStore {
	m := make(map[string]bool, len(presentKeys))
	for _, k := range presentKeys {
		m[k] = true
	}
	return &fakeStore{present: m, errOn: map[string]error{}}
}

func (s *fakeStore) PresignPut(_ context.Context, _, _ string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("fakeStore: PresignPut not implemented")
}

func (s *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	if err, ok := s.errOn[key]; ok {
		return false, err
	}
	return s.present[key], nil
}

func (s *fakeStore) PresignGet(_ context.Context, _ string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("fakeStore: PresignGet not implemented")
}

func (s *fakeStore) Get(_ context.Context, _ string) ([]byte, error) {
	return nil, errors.New("fakeStore: Get not implemented")
}

func (s *fakeStore) Put(_ context.Context, _, _ string, _ []byte) error {
	return errors.New("fakeStore: Put not implemented")
}

// captureLogs redirects the default slog logger into a buffer for the
// duration of the test, restoring the previous default on cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

// schedulerFixture is the seeded state shared by the scheduler tests: two
// tenants at opposite timezone extremes holding work orders with the SAME
// due date, plus an execution in tenant A to anchor photo rows (photo's
// CHECK constraint requires exactly one of execution_id/issue_id/
// resolution_id).
type schedulerFixture struct {
	tenantA     uuid.UUID // Etc/GMT-14 (POSIX sign inversion: UTC+14)
	tenantB     uuid.UUID // Etc/GMT+12 (UTC-12)
	orderA      uuid.UUID // due "yesterday in A's zone" -> must become missed
	orderB      uuid.UUID // SAME due date -> must stay scheduled (control)
	futureOrder uuid.UUID // tenant A, due tomorrow (UTC) -> must stay scheduled
	execution   uuid.UUID // in tenant A, anchors photo rows
}

// seedScheduler seeds the two-tenant timezone control pair.
//
// Discrimination design: tenant A sits at UTC+14 (Etc/GMT-14 — POSIX Etc
// zones invert the sign), tenant B at UTC-12 (Etc/GMT+12). Their local
// clocks are 26h apart, so at ANY instant date_A - date_B is 1 or 2 days.
// Both orders get the SAME due date, computed in SQL as "yesterday in A's
// zone": due = date_A - 1. Then:
//
//	A: due = date_A - 1 < date_A            -> always missed
//	B: due - date_B = (date_A - 1) - date_B = (date_A - date_B) - 1
//	                = {1,2} - 1 = {0,+1}    -> never negative, so the
//	   strict `due_date < tenant-local today` never fires -> stays scheduled
//
// Same due date + different timezone => different outcome is exactly what a
// UTC-naive implementation (one that dropped AT TIME ZONE t.timezone) cannot
// produce — it would mark both or neither. Deterministic at any wall clock,
// unlike a single-tenant `due_date = current_date` probe (a UTC+14 zone's
// local date only rolls ahead of UTC's after 10:00 UTC).
//
// Due dates are computed in SQL (never in Go) so the test also never
// depends on the host machine's local timezone.
func seedScheduler(t *testing.T, pool *pgxpool.Pool) schedulerFixture {
	t.Helper()
	ctx := context.Background()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	f := schedulerFixture{
		tenantA:     uuid.New(),
		tenantB:     uuid.New(),
		orderA:      uuid.New(),
		orderB:      uuid.New(),
		futureOrder: uuid.New(),
		execution:   uuid.New(),
	}
	workerA := uuid.New()

	// Each tenant gets its own object/template/version parents so no row
	// crosses a tenant boundary; the worker user only exists in tenant A
	// (it anchors the execution the photo rows hang off).
	seedTenant := func(tenant uuid.UUID, name, tz string) (object, version uuid.UUID) {
		object, tmpl, version := uuid.New(), uuid.New(), uuid.New()
		must(`INSERT INTO tenant (id, name, timezone) VALUES ($1,$2,$3)`, tenant, name, tz)
		must(`INSERT INTO object (id, tenant_id, name, address) VALUES ($1,$2,'O','A')`, object, tenant)
		must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, tenant)
		must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
		return object, version
	}
	objectA, versionA := seedTenant(f.tenantA, "Ahead", "Etc/GMT-14")
	objectB, versionB := seedTenant(f.tenantB, "Behind", "Etc/GMT+12")
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','W')`, workerA, f.tenantA)

	// The SAME due date for both tenants: yesterday in A's zone.
	const dueYesterdayInA = `(now() AT TIME ZONE 'Etc/GMT-14')::date - 1`
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, due_date, status)
	      VALUES ($1,$2,$3,$4, `+dueYesterdayInA+`, 'scheduled')`, f.orderA, f.tenantA, objectA, versionA)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, due_date, status)
	      VALUES ($1,$2,$3,$4, `+dueYesterdayInA+`, 'scheduled')`, f.orderB, f.tenantB, objectB, versionB)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, due_date, status)
	      VALUES ($1,$2,$3,$4, current_date + 1, 'scheduled')`, f.futureOrder, f.tenantA, objectA, versionA)

	must(`INSERT INTO work_execution (id, tenant_id, work_order_id, worker_id, device_finished_at, finished_at)
	      VALUES ($1,$2,$3,$4, now(), now())`, f.execution, f.tenantA, f.orderA, workerA)

	return f
}

// orderStatus reads a work order's status straight from the DB.
func orderStatus(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) string {
	t.Helper()
	var status string
	if err := pool.QueryRow(context.Background(), `SELECT status FROM work_order WHERE id = $1`, id).Scan(&status); err != nil {
		t.Fatalf("querying order %s status: %v", id, err)
	}
	return status
}

// photoExists reports whether a photo row still exists.
func photoExists(t *testing.T, pool *pgxpool.Pool, id uuid.UUID) bool {
	t.Helper()
	var count int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM photo WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("querying photo %s: %v", id, err)
	}
	return count == 1
}

// insertPhoto seeds an unconfirmed (uploaded_at NULL) photo row anchored to
// the fixture execution, optionally back-dating created_at (the column has
// DEFAULT now(), so age is applied via a direct SQL UPDATE after insert).
func insertPhoto(t *testing.T, pool *pgxpool.Pool, f schedulerFixture, key string, ageDays int) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key) VALUES ($1,$2,$3,'before',$4)`,
		id, f.tenantA, f.execution, key); err != nil {
		t.Fatalf("inserting photo %s: %v", key, err)
	}
	if ageDays > 0 {
		if _, err := pool.Exec(ctx, `UPDATE photo SET created_at = now() - make_interval(days => $2) WHERE id = $1`, id, ageDays); err != nil {
			t.Fatalf("aging photo %s: %v", key, err)
		}
	}
	return id
}

func TestSchedulerRunOnceMarksMissedAndGCsOrphanPhotos(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedScheduler(t, pool)

	oldAbsent := insertPhoto(t, pool, f, "orphan/absent.jpg", 20)   // uploaded_at NULL, key not in store -> deleted
	oldPresent := insertPhoto(t, pool, f, "orphan/present.jpg", 20) // uploaded_at NULL, key in store -> kept + warned
	fresh := insertPhoto(t, pool, f, "orphan/fresh.jpg", 0)         // created just now -> kept (not yet 14d old)

	store := newFakeStore("orphan/present.jpg")
	sched := NewScheduler(pool, store)
	logs := captureLogs(t)

	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// --- missed marking: the two-tenant discrimination pair ---
	if got := orderStatus(t, pool, f.orderA); got != "missed" {
		t.Errorf("tenant A (UTC+14) order status = %q, want missed (due date is yesterday in its zone)", got)
	}
	if got := orderStatus(t, pool, f.orderB); got != "scheduled" {
		t.Errorf("tenant B (UTC-12) order status = %q, want scheduled (same due date is today-or-tomorrow in its zone; a UTC-naive implementation would have marked it)", got)
	}
	if got := orderStatus(t, pool, f.futureOrder); got != "scheduled" {
		t.Errorf("future order status = %q, want scheduled (unchanged)", got)
	}
	if !strings.Contains(logs.String(), "marked overdue orders missed") {
		t.Errorf("logs missing missed-count message, got: %s", logs.String())
	}

	// --- orphan photo GC ---
	if photoExists(t, pool, oldAbsent) {
		t.Error("old-absent orphan photo row still exists, want deleted")
	}
	if !photoExists(t, pool, oldPresent) {
		t.Error("old-present orphan photo row was deleted, want kept (S3 object exists)")
	}
	if !photoExists(t, pool, fresh) {
		t.Error("fresh orphan photo row was deleted, want kept (not yet 14 days old)")
	}
	if !strings.Contains(logs.String(), "orphan photo has S3 object, leaving row") {
		t.Errorf("logs missing present-orphan warn message, got: %s", logs.String())
	}
}

// TestSchedulerGCNeverDeletesOnExistsUncertainty covers the evidence rule
// explicitly: if store.Exists itself errors (S3 unreachable, credentials
// issue, etc.), the row must never be deleted — uncertainty about whether
// the bytes exist is not the same as confirming they don't. The batch also
// holds a healthy deletable orphan AFTER the erroring one (ListOrphanPhotos
// orders by created_at ascending, so the older/erroring row is processed
// first) — its deletion proves one photo's failure never aborts the loop.
// RunOnce must still return nil, and the error must be logged.
func TestSchedulerGCNeverDeletesOnExistsUncertainty(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedScheduler(t, pool)

	uncertain := insertPhoto(t, pool, f, "orphan/uncertain.jpg", 21) // Exists errors -> kept
	deletable := insertPhoto(t, pool, f, "orphan/deletable.jpg", 20) // absent, processed after the error -> deleted

	store := newFakeStore()
	store.errOn["orphan/uncertain.jpg"] = errors.New("s3: connection refused")
	sched := NewScheduler(pool, store)
	logs := captureLogs(t)

	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v, want nil (one photo's failure must not fail the job)", err)
	}

	if !photoExists(t, pool, uncertain) {
		t.Error("photo row was deleted despite an Exists error, want kept (evidence rule)")
	}
	if photoExists(t, pool, deletable) {
		t.Error("deletable orphan after the erroring one still exists, want deleted (loop must continue past a per-photo failure)")
	}
	if !strings.Contains(logs.String(), "s3: connection refused") {
		t.Errorf("logs missing the Exists error, got: %s", logs.String())
	}
}

func TestSchedulerRunOnceNoOrphansIsQuiet(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	seedScheduler(t, pool)

	sched := NewScheduler(pool, newFakeStore())
	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
}
