package report

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
	"sdano.app/api/internal/testdb"
)

// fakeRenderer is a PDFRenderer test double. When err is set, every
// RenderPDF call fails with it; otherwise it returns a fixed PDF payload —
// tests never need a live headless Chrome (task 9 covers that).
type fakeRenderer struct {
	err error
}

func (f *fakeRenderer) RenderPDF(_ context.Context, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	return []byte("%PDF-fake"), nil
}

// fakeStore is a minimal photo.ObjectStore test double backed by an
// in-memory map. These fixtures seed no photos, so only Put (the PDF
// upload) is ever exercised; the presign/Exists/Get methods exist purely to
// satisfy the interface.
type fakeStore struct {
	objects map[string][]byte
}

func newFakeStore() *fakeStore { return &fakeStore{objects: map[string][]byte{}} }

func (s *fakeStore) PresignPut(_ context.Context, _, _ string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("fakeStore: PresignPut not implemented")
}

func (s *fakeStore) Exists(_ context.Context, _ string) (bool, error) { return false, nil }

func (s *fakeStore) PresignGet(_ context.Context, _ string) (string, time.Time, error) {
	return "", time.Time{}, errors.New("fakeStore: PresignGet not implemented")
}

func (s *fakeStore) Get(_ context.Context, key string) ([]byte, error) {
	raw, ok := s.objects[key]
	if !ok {
		return nil, fmt.Errorf("fakeStore: no object %s", key)
	}
	return raw, nil
}

func (s *fakeStore) Put(_ context.Context, key, _ string, body []byte) error {
	s.objects[key] = body
	return nil
}

// seedMinimalReport seeds a minimal renderable report — one tenant, object,
// checklist, work order and execution (no photos, so the worker's PhotoLoader
// is never invoked) — plus one queued report row covering the order's due
// date via InsertReport, per the task-4 brief's step 1.
func seedMinimalReport(t *testing.T, pool *pgxpool.Pool) (tenantID, reportID uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	tenant := uuid.New()
	worker := uuid.New()
	object := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	order, exec := uuid.New(), uuid.New()

	must(`INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','W')`, worker, tenant)
	must(`INSERT INTO object (id, tenant_id, name, address) VALUES ($1,$2,'O','A')`, object, tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status) VALUES ($1,$2,$3,$4,$5,'2026-06-05','done')`,
		order, tenant, object, version, worker)
	must(`INSERT INTO work_execution (id, tenant_id, work_order_id, worker_id, device_finished_at, finished_at) VALUES ($1,$2,$3,$4,'2026-06-05T08:42:00Z','2026-06-05T08:43:00Z')`,
		exec, tenant, order, worker)

	q := db.New(pool)
	inserted, err := q.InsertReport(ctx, db.InsertReportParams{
		TenantID:   tenant,
		PeriodFrom: pgtype.Date{Time: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC), Valid: true},
		PeriodTo:   pgtype.Date{Time: time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC), Valid: true},
	})
	if err != nil {
		t.Fatalf("InsertReport: %v", err)
	}
	return tenant, inserted.ID
}

func TestWorkerTickRendersAndMarksReady(t *testing.T) {
	pool := testdb.New(t)
	tenantID, reportID := seedMinimalReport(t, pool)
	store := newFakeStore()
	w := NewWorker(pool, store, &fakeRenderer{})

	if processed := w.tick(context.Background()); !processed {
		t.Fatal("tick() = false, want true (queue had one row)")
	}

	q := db.New(pool)
	row, err := q.GetReport(context.Background(), db.GetReportParams{ID: reportID, TenantID: tenantID})
	if err != nil {
		t.Fatalf("GetReport: %v", err)
	}
	if row.Status != db.ReportStatusReady {
		t.Fatalf("Status = %q, want ready", row.Status)
	}
	wantKey := fmt.Sprintf("tenants/%s/reports/%s.pdf", tenantID, reportID)
	if row.S3Key == nil || *row.S3Key != wantKey {
		t.Fatalf("S3Key = %v, want %q", row.S3Key, wantKey)
	}
	got, ok := store.objects[wantKey]
	if !ok {
		t.Fatalf("fake store did not receive an object under %q", wantKey)
	}
	if string(got) != "%PDF-fake" {
		t.Errorf("uploaded body = %q, want %%PDF-fake", got)
	}
}

func TestWorkerTickFailsAfterThreeAttempts(t *testing.T) {
	pool := testdb.New(t)
	tenantID, reportID := seedMinimalReport(t, pool)
	store := newFakeStore()
	w := NewWorker(pool, store, &fakeRenderer{err: errors.New("boom")})
	q := db.New(pool)

	// Ticks 1-2: renderer keeps failing, but attempts (1, then 2) stay below
	// the 3-attempt ceiling, so the row must be left 'generating' for retry.
	for i := 1; i <= 2; i++ {
		if processed := w.tick(context.Background()); !processed {
			t.Fatalf("tick %d: processed = false, want true", i)
		}
		row, err := q.GetReport(context.Background(), db.GetReportParams{ID: reportID, TenantID: tenantID})
		if err != nil {
			t.Fatalf("GetReport after tick %d: %v", i, err)
		}
		if row.Status != db.ReportStatusGenerating {
			t.Fatalf("tick %d: Status = %q, want generating (retry left)", i, row.Status)
		}
	}

	// Tick 3: this claim's RenderAttempts is 3, the ceiling — must fail the row.
	if processed := w.tick(context.Background()); !processed {
		t.Fatal("tick 3: processed = false, want true")
	}

	row, err := q.GetReport(context.Background(), db.GetReportParams{ID: reportID, TenantID: tenantID})
	if err != nil {
		t.Fatalf("GetReport after tick 3: %v", err)
	}
	if row.Status != db.ReportStatusFailed {
		t.Fatalf("Status = %q, want failed", row.Status)
	}
	if row.FailureReason == nil || !strings.Contains(*row.FailureReason, "render failed after 3 attempts") {
		t.Errorf("FailureReason = %v, want to contain %q", row.FailureReason, "render failed after 3 attempts")
	}

	var attempts int32
	if err := pool.QueryRow(context.Background(), `SELECT render_attempts FROM report WHERE id = $1`, reportID).Scan(&attempts); err != nil {
		t.Fatalf("querying render_attempts: %v", err)
	}
	if attempts != 3 {
		t.Errorf("render_attempts = %d, want 3", attempts)
	}
	if len(store.objects) != 0 {
		t.Errorf("fake store received %d objects, want 0 (partial failure must never upload)", len(store.objects))
	}
}

func TestWorkerTickEmptyQueueReturnsFalse(t *testing.T) {
	pool := testdb.New(t)
	// No report rows seeded at all.
	w := NewWorker(pool, newFakeStore(), &fakeRenderer{})
	if processed := w.tick(context.Background()); processed {
		t.Error("tick() = true on an empty queue, want false")
	}
}
