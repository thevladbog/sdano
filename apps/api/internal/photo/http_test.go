package photo_test

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/photo"
	"sdano.app/api/internal/testdb"

	"github.com/danielgtaylor/huma/v2/humatest"
)

const testSecret = "photo-test-secret-at-least-32-bytes!!"

// fakeStore records presign calls and answers Exists from a set.
type fakeStore struct {
	exists map[string]bool
}

func (f *fakeStore) PresignPut(_ context.Context, key, _ string) (string, time.Time, error) {
	return "https://s3.example/" + key + "?sig=x", time.Now().Add(15 * time.Minute), nil
}
func (f *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	return f.exists[key], nil
}

// seedExecution inserts a tenant, worker, object, version, order, and an
// execution owned by the worker. Returns tenant, worker, executionID.
func seedExecution(t *testing.T, pool *pgxpool.Pool) (tenant, worker, exec uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, worker, exec = uuid.New(), uuid.New(), uuid.New()
	object, tmpl, version, order := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'O')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,current_date)`, order, tenant, object, version, worker)
	must(`INSERT INTO work_execution (id,tenant_id,work_order_id,worker_id) VALUES ($1,$2,$3,$4)`, exec, tenant, order, worker)
	return tenant, worker, exec
}

// seedSecondExecution adds a second work order + execution for the same
// tenant and worker (a fresh object/template/version, since the schema
// scopes templates and orders to one object). Used to prove a photo-id
// collision across two different executions is rejected rather than
// silently re-pointing the first execution's evidence row.
func seedSecondExecution(t *testing.T, pool *pgxpool.Pool, tenant, worker uuid.UUID) (exec uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	exec = uuid.New()
	object, tmpl, version, order := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'O2')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T2')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,current_date)`, order, tenant, object, version, worker)
	must(`INSERT INTO work_execution (id,tenant_id,work_order_id,worker_id) VALUES ($1,$2,$3,$4)`, exec, tenant, order, worker)
	return exec
}

func buildPhotoAPI(t *testing.T, pool *pgxpool.Pool, store photo.ObjectStore) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t)
	a := auth.NewAuthenticator(testSecret, db.New(pool))
	api.UseMiddleware(a.Authenticate, a.Authorize)
	photo.Register(api, pool, store)
	return api
}

func workerBearer(t *testing.T, tenant, worker uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: worker, TenantID: tenant, Role: auth.RoleWorker}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return "Bearer " + tok
}

func TestPhotoPresignThenConfirm(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	// Presign creates the photo row and returns a URL for the deterministic key.
	presignBody := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"}
	pres := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody)
	if pres.Code != http.StatusOK {
		t.Fatalf("presign: got %d; body %s", pres.Code, pres.Body)
	}
	wantKey := "tenants/" + tenant.String() + "/photos/" + photoID.String() + ".jpg"
	if !strings.Contains(pres.Body.String(), wantKey) {
		t.Errorf("presign body must contain the s3 key %q; body %s", wantKey, pres.Body)
	}

	// Confirm before the object exists → 409 photo-not-uploaded.
	confirmBody := map[string]any{"taken_at": "2026-07-09T09:00:00Z", "lat": 55.75, "lon": 37.61}
	if c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, confirmBody); c.Code != http.StatusConflict || !strings.Contains(c.Body.String(), "photo-not-uploaded") {
		t.Fatalf("confirm before upload: got %d body %s", c.Code, c.Body)
	}

	// Object lands in S3 → confirm succeeds and stamps uploaded_at.
	store.exists[wantKey] = true
	c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, confirmBody)
	if c.Code != http.StatusOK {
		t.Fatalf("confirm: got %d; body %s", c.Code, c.Body)
	}
	var uploaded *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT uploaded_at FROM photo WHERE id=$1`, photoID).Scan(&uploaded); err != nil || uploaded == nil {
		t.Fatalf("uploaded_at not stamped: %v %v", uploaded, err)
	}
	// Re-presign an unconfirmed-or-confirmed photo is idempotent (same key, 200).
	if p2 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody); p2.Code != http.StatusOK {
		t.Errorf("re-presign: got %d", p2.Code)
	}
}

// TestPhotoPresignRejectsIDReuseAcrossExecutions proves that a photo-id
// collision against an existing row bound to a *different* execution is
// rejected, not silently ignored. photo.id is a client-generated, globally
// unique primary key (not scoped per execution or tenant — see db/migrations
// 0001_init.up.sql: "photo.id uuid PRIMARY KEY"), and InsertPhotoPresign
// uses "ON CONFLICT (id) DO NOTHING" for idempotent same-execution replay.
// Without a post-insert ownership check, a colliding id from a second
// execution would silently no-op the insert while still handing back a
// presigned URL — on confirm, ConfirmPhoto would stamp uploaded_at on the
// *first* execution's row even though the uploaded bytes belong to the
// second execution, corrupting evidence attribution (or, if the same S3 key
// is reused, overwriting the first execution's uploaded object outright).
func TestPhotoPresignRejectsIDReuseAcrossExecutions(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec1 := seedExecution(t, pool)
	exec2 := seedSecondExecution(t, pool, tenant, worker)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	body1 := map[string]any{"id": photoID.String(), "execution_id": exec1.String(), "kind": "after", "content_type": "image/jpeg"}
	if p1 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, body1); p1.Code != http.StatusOK {
		t.Fatalf("first presign: got %d; body %s", p1.Code, p1.Body)
	}

	body2 := map[string]any{"id": photoID.String(), "execution_id": exec2.String(), "kind": "after", "content_type": "image/jpeg"}
	p2 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, body2)
	if p2.Code != http.StatusConflict || !strings.Contains(p2.Body.String(), "photo-id-conflict") {
		t.Fatalf("colliding-execution presign: got %d body %s; want 409 photo-id-conflict", p2.Code, p2.Body)
	}

	// The original row must still point at exec1, untouched.
	var gotExec uuid.UUID
	if err := pool.QueryRow(context.Background(), `SELECT execution_id FROM photo WHERE id=$1`, photoID).Scan(&gotExec); err != nil {
		t.Fatalf("reloading photo: %v", err)
	}
	if gotExec != exec1 {
		t.Fatalf("photo row execution_id = %s; want unchanged exec1 %s", gotExec, exec1)
	}
}
