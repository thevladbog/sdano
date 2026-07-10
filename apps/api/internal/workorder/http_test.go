package workorder_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

const testSecret = "worker-test-secret-at-least-32-bytes!!"

func workerBearer(t *testing.T, tenant, worker uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: worker, TenantID: tenant, Role: auth.RoleWorker}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return "Bearer " + tok
}

func TestWorkerTodayReturnsAssignedRoute(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	tenant, worker := uuid.New(), uuid.New()
	object := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	item1, item2 := uuid.New(), uuid.New()
	order := uuid.New()
	otherWorker := uuid.New()

	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Alexey')`, worker, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Other')`, otherWorker, tenant)
	must(`INSERT INTO object (id, tenant_id, name, address, qr_token) VALUES ($1,$2,'Lenina 45','Lenina 45','QR-LENINA')`, object, tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'Bus stop')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title, requires_photo) VALUES ($1,$2,1,'Collect trash',false)`, item1, version)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title, requires_photo) VALUES ($1,$2,2,'Wash shelter',true)`, item2, version)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, order, tenant, object, version, worker, today)
	// A second order for another worker on the same day must NOT appear.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, uuid.New(), tenant, object, version, otherWorker, today)

	// Cross-tenant isolation: prove another tenant's order does NOT appear.
	otherTenant := uuid.New()
	otherWorkerForOtherTenant := uuid.New()
	otherObject := uuid.New()
	otherTmpl, otherVersion, otherOrder := uuid.New(), uuid.New(), uuid.New()
	must(`INSERT INTO tenant (id, name) VALUES ($1, 'OtherCo')`, otherTenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Ivan')`, otherWorkerForOtherTenant, otherTenant)
	must(`INSERT INTO object (id, tenant_id, name) VALUES ($1,$2,'OTHER-TENANT-OBJECT')`, otherObject, otherTenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'OtherT')`, otherTmpl, otherTenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, otherVersion, otherTmpl)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, otherOrder, otherTenant, otherObject, otherVersion, otherWorkerForOtherTenant, today)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/today", nil)
	req.Header.Set("Authorization", workerBearer(t, tenant, worker))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("today: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Lenina 45", "QR-LENINA", "Collect trash", "Wash shelter", `"version_id"`, order.String()} {
		if !strings.Contains(body, want) {
			t.Errorf("today body missing %q; body: %s", want, body)
		}
	}
	// Cross-tenant isolation: other tenant's object must not appear.
	if strings.Contains(body, "OTHER-TENANT-OBJECT") {
		t.Errorf("tenant isolation broken — another tenant's object leaked into /worker/today; body: %s", body)
	}
	// The other worker's route count: exactly one work_order for this worker.
	if n := strings.Count(body, `"object_id"`); n != 1 {
		t.Errorf("expected exactly 1 work order for this worker, saw %d; body: %s", n, body)
	}
}

func TestExecutionUpsertHTTPRoundTrip(t *testing.T) {
	pool := testdb.New(t)
	worker := uuid.New()
	f := seedExecutionFixture(t, pool, worker)
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	execID := uuid.New()

	body := `{"work_order_id":"` + f.order.String() + `","started_at":"2026-07-09T09:00:00Z","items":[{"id":"` + f.execItem1.String() + `","template_item_id":"` + f.tmplItem1.String() + `","checked":true}]}`
	put := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/worker/executions/"+execID.String(), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := put(workerBearer(t, f.tenant, worker))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: got %d; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), f.execItem1.String()) {
		t.Errorf("server view must echo the item; body %s", rec.Body)
	}
	// Replay is safe (idempotent) → still 200.
	if rec2 := put(workerBearer(t, f.tenant, worker)); rec2.Code != http.StatusOK {
		t.Errorf("replay: got %d", rec2.Code)
	}
	// A different worker in the same tenant is forbidden (order not theirs).
	intruder := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','I')`, intruder, f.tenant); err != nil {
		t.Fatalf("seed intruder: %v", err)
	}
	if rec3 := put(workerBearer(t, f.tenant, intruder)); rec3.Code != http.StatusForbidden {
		t.Errorf("intruder: got %d, want 403; body %s", rec3.Code, rec3.Body)
	}
}

func TestWorkerTodayUsesTenantTimezone(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, worker := uuid.New(), uuid.New()
	object, tmpl, version, order := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	// Kiritimati is UTC+14: its calendar date is ahead of UTC for 10h/day, so
	// this test deterministically diverges from UTC whenever UTC time is >= 10:00.
	// To be deterministic at ANY hour, compute the tenant-local date in Go and
	// seed the order on that date; assert it is returned.
	must(`INSERT INTO tenant (id, name, timezone) VALUES ($1,'TZ Co','Pacific/Kiritimati')`, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id, tenant_id, name) VALUES ($1,$2,'O')`, object, tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	loc, err := time.LoadLocation("Pacific/Kiritimati")
	if err != nil {
		t.Fatalf("load location: %v", err)
	}
	localToday := time.Now().In(loc).Format("2006-01-02")
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, order, tenant, object, version, worker, localToday)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/today", nil)
	req.Header.Set("Authorization", workerBearer(t, tenant, worker))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("today: got %d; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), order.String()) {
		t.Errorf("order due on the tenant-local date must be returned; body: %s", rec.Body)
	}
}

func TestStaffWorkOrdersBulkCreateListPatch(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, worker := uuid.New(), uuid.New()
	object, tmpl, version := uuid.New(), uuid.New(), uuid.New()
	otherTenantObject := uuid.New()
	otherTenant := uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Other')`, otherTenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'O')`, object, tenant)
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'Foreign')`, otherTenantObject, otherTenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearerAs2(t, tenant, uuid.New(), auth.RoleAdmin)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		var rdr io.Reader
		if body != "" {
			rdr = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, path, rdr)
		req.Header.Set("Authorization", admin)
		if body != "" {
			req.Header.Set("Content-Type", "application/json")
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	// Bulk create: two orders for the week.
	bulk := `[{"object_id":"` + object.String() + `","version_id":"` + version.String() + `","assignee_id":"` + worker.String() + `","due_date":"2026-07-13"},
	         {"object_id":"` + object.String() + `","version_id":"` + version.String() + `","due_date":"2026-07-14"}]`
	rec := do(http.MethodPost, "/api/v1/staff/work-orders", bulk)
	if rec.Code != http.StatusCreated {
		t.Fatalf("bulk create: got %d; body %s", rec.Code, rec.Body)
	}
	// A cross-tenant object_id fails the WHOLE batch atomically (422), nothing created.
	before := countOrders(t, pool, tenant)
	bad := `[{"object_id":"` + object.String() + `","version_id":"` + version.String() + `","due_date":"2026-07-15"},
	        {"object_id":"` + otherTenantObject.String() + `","version_id":"` + version.String() + `","due_date":"2026-07-15"}]`
	if rec = do(http.MethodPost, "/api/v1/staff/work-orders", bad); rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid-reference") {
		t.Fatalf("cross-tenant batch: got %d; body %s", rec.Code, rec.Body)
	}
	if after := countOrders(t, pool, tenant); after != before {
		t.Errorf("failed batch must create nothing: before=%d after=%d", before, after)
	}
	// A literal JSON `null` body satisfies the generated ["array","null"]
	// schema and bypasses minItems, so the handler must reject it explicitly
	// instead of silently 201-ing with created:0.
	beforeNull := countOrders(t, pool, tenant)
	if rec = do(http.MethodPost, "/api/v1/staff/work-orders", "null"); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("null body: got %d; body %s", rec.Code, rec.Body)
	}
	if after := countOrders(t, pool, tenant); after != beforeNull {
		t.Errorf("null body must create nothing: before=%d after=%d", beforeNull, after)
	}
	// List by date.
	rec = do(http.MethodGet, "/api/v1/staff/work-orders?date=2026-07-13", "")
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), `"object_id"`) != 1 {
		t.Fatalf("list: got %d; body %s", rec.Code, rec.Body)
	}
	orderID := extractID2(t, rec.Body.String())
	// Patch: reassign + reschedule.
	rec = do(http.MethodPatch, "/api/v1/staff/work-orders/"+orderID, `{"due_date":"2026-07-20"}`)
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "2026-07-20") {
		t.Fatalf("patch: got %d; body %s", rec.Code, rec.Body)
	}
	// Patch with a cross-tenant assignee -> 422 invalid-reference.
	otherWorker := uuid.New()
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','X')`, otherWorker, otherTenant)
	if rec = do(http.MethodPatch, "/api/v1/staff/work-orders/"+orderID, `{"assignee_id":"`+otherWorker.String()+`"}`); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("cross-tenant assignee: got %d; body %s", rec.Code, rec.Body)
	}
}

func countOrders(t *testing.T, pool *pgxpool.Pool, tenant uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(), `SELECT count(*) FROM work_order WHERE tenant_id=$1`, tenant).Scan(&n); err != nil {
		t.Fatalf("count: %v", err)
	}
	return n
}

func extractID2(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `"id":"`)
	if i < 0 {
		t.Fatalf("no id in %s", body)
	}
	rest := body[i+6:]
	return rest[:strings.IndexByte(rest, '"')]
}

func bearerAs2(t *testing.T, tenant, user uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: user, TenantID: tenant, Role: role}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return "Bearer " + tok
}
