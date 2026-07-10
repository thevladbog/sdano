package photo_test

import (
	"context"
	"encoding/json"
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
	"sdano.app/api/internal/workorder"

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
func (f *fakeStore) PresignGet(_ context.Context, key string) (string, time.Time, error) {
	return "https://s3.example/GET/" + key + "?sig=g", time.Now().Add(5 * time.Minute), nil
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
	// Re-presigning an already-CONFIRMED photo must be rejected: the uploaded
	// evidence bytes must not be replaceable. (Re-presigning an UNCONFIRMED
	// photo — uploaded_at still null — remains allowed as the resumability
	// path for a client retrying an interrupted upload before confirm.)
	p2 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody)
	if p2.Code != http.StatusConflict || !strings.Contains(p2.Body.String(), "photo-already-uploaded") {
		t.Errorf("re-presign of confirmed photo: got %d body %s; want 409 photo-already-uploaded", p2.Code, p2.Body)
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

// TestPhotoPresignRejectsKindReuse proves that a photo-id collision on the
// SAME execution but with a DIFFERENT kind is rejected, not silently
// accepted. InsertPhotoPresign's ON CONFLICT (id) DO NOTHING also no-ops in
// this case, which — absent this check — would hand back a fresh presigned
// URL while the row silently kept its original kind, mislabeling the
// evidence that eventually lands at this id.
func TestPhotoPresignRejectsKindReuse(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	before := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "before", "content_type": "image/jpeg"}
	if p1 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, before); p1.Code != http.StatusOK {
		t.Fatalf("first presign: got %d; body %s", p1.Code, p1.Body)
	}

	after := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"}
	p2 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, after)
	if p2.Code != http.StatusConflict || !strings.Contains(p2.Body.String(), "photo-id-conflict") {
		t.Fatalf("kind-reuse presign: got %d body %s; want 409 photo-id-conflict", p2.Code, p2.Body)
	}

	var gotKind string
	if err := pool.QueryRow(context.Background(), `SELECT kind FROM photo WHERE id=$1`, photoID).Scan(&gotKind); err != nil {
		t.Fatalf("reloading photo: %v", err)
	}
	if gotKind != "before" {
		t.Fatalf("photo row kind = %q; want unchanged \"before\"", gotKind)
	}
}

// TestPhotoConfirmRejectsNonOwningWorker proves that confirm — like presign —
// verifies the calling worker owns the photo's execution. GetPhoto is only
// tenant-scoped, so without this check any worker in the tenant could confirm
// (and thus stamp taken_at/lat/lon evidence for) another worker's photo.
func TestPhotoConfirmRejectsNonOwningWorker(t *testing.T) {
	pool := testdb.New(t)
	tenant, workerA, exec := seedExecution(t, pool)
	workerB := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','B')`, workerB, tenant); err != nil {
		t.Fatalf("seed workerB: %v", err)
	}
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearerA := workerBearer(t, tenant, workerA)
	bearerB := workerBearer(t, tenant, workerB)
	photoID := uuid.New()

	presignBody := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"}
	if p := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearerA, presignBody); p.Code != http.StatusOK {
		t.Fatalf("presign: got %d; body %s", p.Code, p.Body)
	}
	wantKey := "tenants/" + tenant.String() + "/photos/" + photoID.String() + ".jpg"
	store.exists[wantKey] = true

	confirmBody := map[string]any{"taken_at": "2026-07-09T09:00:00Z", "lat": 55.75, "lon": 37.61}
	c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearerB, confirmBody)
	if c.Code != http.StatusForbidden || !strings.Contains(c.Body.String(), "work-order-not-assigned") {
		t.Fatalf("confirm by non-owning worker: got %d body %s; want 403 work-order-not-assigned", c.Code, c.Body)
	}
}

// TestPhotoConfirmIsEvidenceImmutable proves a re-confirm cannot overwrite
// already-stamped taken_at/lat/lon: ConfirmPhoto uses COALESCE so the first
// non-null value sticks, matching uploaded_at's existing once-only semantics.
func TestPhotoConfirmIsEvidenceImmutable(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	presignBody := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"}
	if p := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody); p.Code != http.StatusOK {
		t.Fatalf("presign: got %d; body %s", p.Code, p.Body)
	}
	wantKey := "tenants/" + tenant.String() + "/photos/" + photoID.String() + ".jpg"
	store.exists[wantKey] = true

	first := map[string]any{"taken_at": "2026-07-09T09:00:00Z", "lat": 55.75, "lon": 37.61}
	if c1 := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, first); c1.Code != http.StatusOK {
		t.Fatalf("first confirm: got %d; body %s", c1.Code, c1.Body)
	}

	second := map[string]any{"taken_at": "2020-01-01T00:00:00Z", "lat": 0.0, "lon": 0.0}
	c2 := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, second)
	if c2.Code != http.StatusOK {
		t.Fatalf("second confirm: got %d; body %s", c2.Code, c2.Body)
	}

	var lat, lon *float64
	if err := pool.QueryRow(context.Background(), `SELECT lat, lon FROM photo WHERE id=$1`, photoID).Scan(&lat, &lon); err != nil {
		t.Fatalf("reloading photo: %v", err)
	}
	if lat == nil || *lat != 55.75 || lon == nil || *lon != 37.61 {
		t.Fatalf("lat/lon overwritten by second confirm: lat=%v lon=%v; want unchanged 55.75/37.61", lat, lon)
	}
}

// TestPhotoPresignRejectsNonJpeg proves that presign only accepts
// image/jpeg: the S3 key is always suffixed .jpg and the mobile client only
// ever sends JPEG, so any other content type is rejected up front rather
// than silently presigning a URL for a mismatched extension.
func TestPhotoPresignRejectsNonJpeg(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	presignBody := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "text/html"}
	p := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody)
	if p.Code != http.StatusUnprocessableEntity || !strings.Contains(p.Body.String(), "unsupported-content-type") {
		t.Fatalf("non-jpeg presign: got %d body %s; want 422 unsupported-content-type", p.Code, p.Body)
	}
}

// TestStaffPhotoURLAndExecutionDetail proves the staff evidence-read surface:
// a presigned GET for a confirmed photo, a loud 409 for an unconfirmed one,
// and an execution detail view whose photo list flags upload state per photo
// (never silently dropping the unconfirmed one) rather than only listing
// confirmed evidence.
func TestStaffPhotoURLAndExecutionDetail(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	// Full app (staff exec detail lives in workorder pkg) — build via app.New?
	// app.New wires the REAL S3Store; for fake-store staff routes use humatest:
	_, api := humatest.New(t)
	a := auth.NewAuthenticator(testSecret, db.New(pool))
	api.UseMiddleware(a.Authenticate, a.Authorize)
	photo.Register(api, pool, store)
	workorder.Register(api, pool, store)

	adminTok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: uuid.New(), TenantID: tenant, Role: auth.RoleAdmin}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	admin := "Authorization: Bearer " + adminTok

	// Seed a confirmed photo via the worker flow.
	photoID := uuid.New()
	wtok, _ := auth.IssueAccessToken(testSecret, auth.Principal{UserID: worker, TenantID: tenant, Role: auth.RoleWorker}, auth.AccessTTL)
	wb := "Authorization: Bearer " + wtok
	pres := api.Post("/api/v1/worker/photos/presign", wb, map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"})
	if pres.Code != http.StatusOK {
		t.Fatalf("presign: %d %s", pres.Code, pres.Body)
	}
	wantKey := "tenants/" + tenant.String() + "/photos/" + photoID.String() + ".jpg"
	store.exists[wantKey] = true
	if c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", wb, map[string]any{"taken_at": "2026-07-10T09:00:00Z"}); c.Code != http.StatusOK {
		t.Fatalf("confirm: %d %s", c.Code, c.Body)
	}

	// Staff photo URL: presigned GET for a confirmed photo.
	rec := api.Get("/api/v1/staff/photos/"+photoID.String()+"/url", admin)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "https://s3.example/GET/"+wantKey) {
		t.Fatalf("photo url: got %d; body %s", rec.Code, rec.Body)
	}
	// Unconfirmed photo -> 409 photo-not-uploaded.
	photo2 := uuid.New()
	if p2 := api.Post("/api/v1/worker/photos/presign", wb, map[string]any{"id": photo2.String(), "execution_id": exec.String(), "kind": "before", "content_type": "image/jpeg"}); p2.Code != http.StatusOK {
		t.Fatalf("presign2: %d", p2.Code)
	}
	if rec = api.Get("/api/v1/staff/photos/"+photo2.String()+"/url", admin); rec.Code != http.StatusConflict {
		t.Errorf("unconfirmed url: got %d, want 409", rec.Code)
	}

	// Execution detail: items with titles + photos (confirmed with URL, unconfirmed flagged).
	rec = api.Get("/api/v1/staff/executions/"+exec.String(), admin)
	if rec.Code != http.StatusOK {
		t.Fatalf("exec detail: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{`"worker_name"`, photoID.String(), `"uploaded":true`, photo2.String(), `"uploaded":false`} {
		if !strings.Contains(body, want) {
			t.Errorf("exec detail missing %q; body: %s", want, body)
		}
	}
	// The unconfirmed photo must carry no url key at all (not even url:null —
	// the schema promises the key only appears once bytes are in S3), while
	// the confirmed one must have it.
	var detail struct {
		Photos []map[string]json.RawMessage `json:"photos"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &detail); err != nil {
		t.Fatalf("decoding exec detail: %v", err)
	}
	for _, ph := range detail.Photos {
		var id string
		if err := json.Unmarshal(ph["id"], &id); err != nil {
			t.Fatalf("decoding photo id: %v", err)
		}
		_, hasURL := ph["url"]
		switch id {
		case photoID.String():
			if !hasURL {
				t.Error("confirmed photo must carry a url")
			}
		case photo2.String():
			if hasURL {
				t.Errorf("unconfirmed photo must not carry a url key; got %s", ph["url"])
			}
		}
	}
	// Unknown execution -> 404.
	if rec = api.Get("/api/v1/staff/executions/"+uuid.NewString(), admin); rec.Code != http.StatusNotFound {
		t.Errorf("unknown exec: got %d, want 404", rec.Code)
	}
}
