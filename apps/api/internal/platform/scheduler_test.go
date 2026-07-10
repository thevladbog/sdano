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
// report package's fakeStore.
type fakeStore struct {
	present   map[string]bool
	existsErr error
}

func newFakeStore(presentKeys ...string) *fakeStore {
	m := make(map[string]bool, len(presentKeys))
	for _, k := range presentKeys {
		m[k] = true
	}
	return &fakeStore{present: m}
}

func (s *fakeStore) PresignPut(_ context.Context, _, _ string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("fakeStore: PresignPut not implemented")
}

func (s *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	if s.existsErr != nil {
		return false, s.existsErr
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

// seedScheduler seeds one tenant on Pacific/Kiritimati (UTC+14 — the
// tenant's local date is already ahead of the DB's UTC current_date), one
// past-due and one future-due scheduled work order (due dates computed in
// SQL via current_date so the test never depends on the host's local
// timezone), and an execution to anchor the photo rows the caller seeds
// separately (photo's CHECK constraint requires exactly one of
// execution_id/issue_id/resolution_id).
func seedScheduler(t *testing.T, pool *pgxpool.Pool) (tenantID, pastOrderID, futureOrderID, executionID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	tenant := uuid.New()
	workerUser := uuid.New()
	object := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	pastOrder, futureOrder := uuid.New(), uuid.New()
	exec := uuid.New()

	must(`INSERT INTO tenant (id, name, timezone) VALUES ($1, 'Acme', 'Pacific/Kiritimati')`, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','W')`, workerUser, tenant)
	must(`INSERT INTO object (id, tenant_id, name, address) VALUES ($1,$2,'O','A')`, object, tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)

	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status)
	      VALUES ($1,$2,$3,$4,$5, current_date - 1, 'scheduled')`, pastOrder, tenant, object, version, workerUser)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status)
	      VALUES ($1,$2,$3,$4,$5, current_date + 1, 'scheduled')`, futureOrder, tenant, object, version, workerUser)

	must(`INSERT INTO work_execution (id, tenant_id, work_order_id, worker_id, device_finished_at, finished_at)
	      VALUES ($1,$2,$3,$4, now(), now())`, exec, tenant, pastOrder, workerUser)

	return tenant, pastOrder, futureOrder, exec
}

func TestSchedulerRunOnceMarksMissedAndGCsOrphanPhotos(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, pastOrder, futureOrder, exec := seedScheduler(t, pool)

	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	insertPhoto := func(key string, ageDays int) uuid.UUID {
		id := uuid.New()
		must(`INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key) VALUES ($1,$2,$3,'before',$4)`,
			id, tenant, exec, key)
		if ageDays > 0 {
			must(`UPDATE photo SET created_at = now() - make_interval(days => $2) WHERE id = $1`, id, ageDays)
		}
		return id
	}

	oldAbsent := insertPhoto("orphan/absent.jpg", 20)  // uploaded_at NULL, key not in store -> deleted
	oldPresent := insertPhoto("orphan/present.jpg", 20) // uploaded_at NULL, key in store -> kept + warned
	fresh := insertPhoto("orphan/fresh.jpg", 0)         // created just now -> kept (not yet 14d old)

	store := newFakeStore("orphan/present.jpg")
	sched := NewScheduler(pool, store)
	logs := captureLogs(t)

	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}

	// --- missed marking ---
	var pastStatus, futureStatus string
	if err := pool.QueryRow(ctx, `SELECT status FROM work_order WHERE id = $1`, pastOrder).Scan(&pastStatus); err != nil {
		t.Fatalf("querying past order status: %v", err)
	}
	if pastStatus != "missed" {
		t.Errorf("past order status = %q, want missed", pastStatus)
	}
	if err := pool.QueryRow(ctx, `SELECT status FROM work_order WHERE id = $1`, futureOrder).Scan(&futureStatus); err != nil {
		t.Fatalf("querying future order status: %v", err)
	}
	if futureStatus != "scheduled" {
		t.Errorf("future order status = %q, want scheduled (unchanged)", futureStatus)
	}
	if !strings.Contains(logs.String(), "marked overdue orders missed") {
		t.Errorf("logs missing missed-count message, got: %s", logs.String())
	}

	// --- orphan photo GC ---
	photoExists := func(id uuid.UUID) bool {
		t.Helper()
		var count int
		if err := pool.QueryRow(ctx, `SELECT count(*) FROM photo WHERE id = $1`, id).Scan(&count); err != nil {
			t.Fatalf("querying photo %s: %v", id, err)
		}
		return count == 1
	}
	if photoExists(oldAbsent) {
		t.Error("old-absent orphan photo row still exists, want deleted")
	}
	if !photoExists(oldPresent) {
		t.Error("old-present orphan photo row was deleted, want kept (S3 object exists)")
	}
	if !photoExists(fresh) {
		t.Error("fresh orphan photo row was deleted, want kept (not yet 14 days old)")
	}
	if !strings.Contains(logs.String(), "orphan photo has S3 object, leaving row") {
		t.Errorf("logs missing present-orphan warn message, got: %s", logs.String())
	}
}

// TestSchedulerGCNeverDeletesOnExistsUncertainty covers the evidence rule
// explicitly: if store.Exists itself errors (S3 unreachable, credentials
// issue, etc.), the row must never be deleted — uncertainty about whether
// the bytes exist is not the same as confirming they don't. RunOnce must
// still return nil (a single photo's GC failure never aborts the loop or
// fails the job), and the error must be logged.
func TestSchedulerGCNeverDeletesOnExistsUncertainty(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, _, _, exec := seedScheduler(t, pool)

	id := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key) VALUES ($1,$2,$3,'before',$4)`,
		id, tenant, exec, "orphan/uncertain.jpg"); err != nil {
		t.Fatalf("inserting photo: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE photo SET created_at = now() - interval '20 days' WHERE id = $1`, id); err != nil {
		t.Fatalf("aging photo: %v", err)
	}

	store := &fakeStore{present: map[string]bool{}, existsErr: errors.New("s3: connection refused")}
	sched := NewScheduler(pool, store)
	logs := captureLogs(t)

	if err := sched.RunOnce(ctx); err != nil {
		t.Fatalf("RunOnce: %v, want nil (one photo's failure must not fail the job)", err)
	}

	var count int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM photo WHERE id = $1`, id).Scan(&count); err != nil {
		t.Fatalf("querying photo: %v", err)
	}
	if count != 1 {
		t.Error("photo row was deleted despite an Exists error, want kept (evidence rule)")
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
