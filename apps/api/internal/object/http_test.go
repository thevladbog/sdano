package object_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

const testSecret = "test-secret"

func bearer(t *testing.T, tenant uuid.UUID, role auth.Role) string {
	t.Helper()
	return bearerAs(t, tenant, uuid.New(), role)
}

// bearerAs signs a token for an explicit user id, needed when a test asserts
// on data keyed by the caller's UserID (e.g. work orders assigned to a
// specific worker).
func bearerAs(t *testing.T, tenant, user uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: user, TenantID: tenant, Role: role}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return "Bearer " + tok
}

func TestListObjectsIsTenantScoped(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	var tenantA, tenantB uuid.UUID
	for _, row := range []struct {
		id   *uuid.UUID
		name string
	}{{&tenantA, "A"}, {&tenantB, "B"}} {
		*row.id = uuid.New()
		if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, $2)`, *row.id, row.name); err != nil {
			t.Fatalf("insert tenant %s: %v", row.name, err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO object (tenant_id, name) VALUES ($1, 'Lenina 45 — bus stop')`, tenantA)
	mustExec(`INSERT INTO object (tenant_id, name) VALUES ($1, 'Other tenant object')`, tenantB)
	mustExec(`INSERT INTO object (tenant_id, name, is_active) VALUES ($1, 'Retired stop', false)`, tenantA)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})

	get := func(authz, path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	const listPath = "/api/v1/staff/objects"

	// No token → 401.
	if rec := get("", listPath); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401; body: %s", rec.Code, rec.Body)
	}
	// A worker token is authenticated but forbidden on a staff route → 403.
	if rec := get(bearer(t, tenantA, auth.RoleWorker), listPath); rec.Code != http.StatusForbidden {
		t.Fatalf("worker on staff route: got %d, want 403; body: %s", rec.Code, rec.Body)
	}
	admin := bearer(t, tenantA, auth.RoleAdmin)
	// Unfiltered: tenant A admin sees ALL its objects, active and inactive —
	// the default is "everything for this tenant", not "active only".
	rec := get(admin, listPath)
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant A: got %d; body: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lenina 45") {
		t.Errorf("tenant A must see its object; body: %s", body)
	}
	if strings.Contains(body, "Other tenant object") {
		t.Errorf("tenant isolation broken — tenant B object leaked; body: %s", body)
	}
	if !strings.Contains(body, "Retired stop") {
		t.Errorf("unfiltered list must include inactive objects; body: %s", body)
	}
	// ?active=true filters the retired object out but keeps the live one and
	// the tenant isolation guarantee.
	recActive := get(admin, listPath+"?active=true")
	if recActive.Code != http.StatusOK {
		t.Fatalf("active filter: got %d; body: %s", recActive.Code, recActive.Body)
	}
	activeBody := recActive.Body.String()
	if !strings.Contains(activeBody, "Lenina 45") {
		t.Errorf("?active=true must keep the live object; body: %s", activeBody)
	}
	if strings.Contains(activeBody, "Retired stop") {
		t.Errorf("?active=true must filter out inactive objects; body: %s", activeBody)
	}
	if strings.Contains(activeBody, "Other tenant object") {
		t.Errorf("tenant isolation broken under ?active=true — tenant B object leaked; body: %s", activeBody)
	}
	// ?active=false is the mirror: only the retired object.
	recInactive := get(admin, listPath+"?active=false")
	if recInactive.Code != http.StatusOK {
		t.Fatalf("inactive filter: got %d; body: %s", recInactive.Code, recInactive.Body)
	}
	inactiveBody := recInactive.Body.String()
	if !strings.Contains(inactiveBody, "Retired stop") {
		t.Errorf("?active=false must keep the retired object; body: %s", inactiveBody)
	}
	if strings.Contains(inactiveBody, "Lenina 45") {
		t.Errorf("?active=false must filter out active objects; body: %s", inactiveBody)
	}
}

func TestListObjectsFiltersByContract(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	contractA, contractB := uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO contract (id,tenant_id,name) VALUES ($1,$2,'Contract A')`, contractA, tenant)
	must(`INSERT INTO contract (id,tenant_id,name) VALUES ($1,$2,'Contract B')`, contractB, tenant)
	must(`INSERT INTO object (tenant_id,name,contract_id) VALUES ($1,'Under contract A',$2)`, tenant, contractA)
	must(`INSERT INTO object (tenant_id,name,contract_id) VALUES ($1,'Under contract B',$2)`, tenant, contractB)
	must(`INSERT INTO object (tenant_id,name) VALUES ($1,'No contract')`, tenant)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearer(t, tenant, auth.RoleAdmin)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	rec := get("/api/v1/staff/objects?contract_id=" + contractA.String())
	if rec.Code != http.StatusOK {
		t.Fatalf("filter by contract: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Under contract A") {
		t.Errorf("must include the object under contract A; body: %s", body)
	}
	if strings.Contains(body, "Under contract B") || strings.Contains(body, "No contract") {
		t.Errorf("must exclude objects not under contract A; body: %s", body)
	}
	// Garbage contract_id -> 422 invalid-reference.
	if bad := get("/api/v1/staff/objects?contract_id=not-a-uuid"); bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad contract_id: got %d, want 422; body %s", bad.Code, bad.Body)
	}
}

func TestWorkerObjectByQR(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	tenant, worker := uuid.New(), uuid.New()
	object := uuid.New()
	tmpl, version, order := uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name,qr_token) VALUES ($1,$2,'Lenina 45','QR-XYZ')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, order, tenant, object, version, worker, today)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/objects/by-qr/"+token, nil)
		req.Header.Set("Authorization", bearerAs(t, tenant, worker, auth.RoleWorker))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := get("QR-XYZ")
	if rec.Code != http.StatusOK {
		t.Fatalf("qr resolve: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lenina 45") || !strings.Contains(body, order.String()) {
		t.Errorf("qr body must carry the object and today's order; body %s", body)
	}
	// Unknown QR → 404.
	if rec404 := get("QR-NOPE"); rec404.Code != http.StatusNotFound {
		t.Errorf("unknown qr: got %d, want 404", rec404.Code)
	}
}

func TestWorkerObjectByQRNoOrderToday(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, worker := uuid.New(), uuid.New()
	object := uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name,qr_token) VALUES ($1,$2,'Lenina 45','QR-NO-ORDER')`, object, tenant)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/objects/by-qr/"+token, nil)
		req.Header.Set("Authorization", bearerAs(t, tenant, worker, auth.RoleWorker))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := get("QR-NO-ORDER")
	if rec.Code != http.StatusOK {
		t.Fatalf("qr resolve: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lenina 45") {
		t.Errorf("qr body must carry the object; body %s", body)
	}
	if strings.Contains(body, "today_work_order") {
		t.Errorf("today_work_order should be omitted when no order due today; body %s", body)
	}
}

func TestWorkerObjectByQRRejectsInactiveObject(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, worker, object := uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec: %v", err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name,qr_token,is_active) VALUES ($1,$2,'Dead stop','QR-DEAD',false)`, object, tenant)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/objects/by-qr/QR-DEAD", nil)
	req.Header.Set("Authorization", bearerAs(t, tenant, worker, auth.RoleWorker))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("inactive object QR: got %d, want 404; body %s", rec.Code, rec.Body)
	}
}

func TestStaffObjectCRUDAndCard(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearerAs(t, tenant, uuid.New(), auth.RoleAdmin)
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
	// Create.
	rec := do(http.MethodPost, "/api/v1/staff/objects", `{"name":"Lenina 45","address":"Lenina 45","qr_token":"QR-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body %s", rec.Code, rec.Body)
	}
	id := extractID(t, rec.Body.String())
	// Patch: deactivate.
	if rec = do(http.MethodPatch, "/api/v1/staff/objects/"+id, `{"is_active":false}`); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"is_active":false`) {
		t.Fatalf("patch: got %d; body %s", rec.Code, rec.Body)
	}
	// Card still readable for inactive objects.
	if rec = do(http.MethodGet, "/api/v1/staff/objects/"+id, ""); rec.Code != http.StatusOK {
		t.Fatalf("card: got %d; body %s", rec.Code, rec.Body)
	}
	// Unknown id -> 404 object-not-found.
	if rec = do(http.MethodGet, "/api/v1/staff/objects/"+uuid.NewString(), ""); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown card: got %d", rec.Code)
	}
	// Worker role on staff route -> 403 (middleware).
	req := httptest.NewRequest(http.MethodPost, "/api/v1/staff/objects", strings.NewReader(`{"name":"X"}`))
	req.Header.Set("Authorization", bearerAs(t, tenant, uuid.New(), auth.RoleWorker))
	req.Header.Set("Content-Type", "application/json")
	rec2 := httptest.NewRecorder()
	router.ServeHTTP(rec2, req)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("worker create: got %d, want 403", rec2.Code)
	}
}

func TestStaffObjectRejectsCrossTenantContract(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, otherTenant := uuid.New(), uuid.New()
	otherContract := uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO tenant (id,name) VALUES ($1,'OtherCo')`, otherTenant)
	must(`INSERT INTO contract (id, tenant_id, name) VALUES ($1, $2, 'Other tenant contract')`, otherContract, otherTenant)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearerAs(t, tenant, uuid.New(), auth.RoleAdmin)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", admin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// Create with another tenant's contract_id -> 422 invalid-reference.
	createBody := `{"name":"Lenina 45","contract_id":"` + otherContract.String() + `"}`
	if rec := do(http.MethodPost, "/api/v1/staff/objects", createBody); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("create with cross-tenant contract: got %d, want 422; body %s", rec.Code, rec.Body)
	}

	// A plain create (no contract) to have an id to patch against.
	rec := do(http.MethodPost, "/api/v1/staff/objects", `{"name":"Lenina 45"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("plain create: got %d; body %s", rec.Code, rec.Body)
	}
	id := extractID(t, rec.Body.String())

	// Patch with another tenant's contract_id -> 422 invalid-reference.
	patchBody := `{"contract_id":"` + otherContract.String() + `"}`
	if rec := do(http.MethodPatch, "/api/v1/staff/objects/"+id, patchBody); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch with cross-tenant contract: got %d, want 422; body %s", rec.Code, rec.Body)
	}

	// A nonexistent contract_id must also 422, not 500 on the raw FK violation.
	nonexistentBody := `{"contract_id":"` + uuid.NewString() + `"}`
	if rec := do(http.MethodPatch, "/api/v1/staff/objects/"+id, nonexistentBody); rec.Code != http.StatusUnprocessableEntity {
		t.Fatalf("patch with unknown contract: got %d, want 422; body %s", rec.Code, rec.Body)
	}
}

func TestStaffObjectQRTokenConflict(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearerAs(t, tenant, uuid.New(), auth.RoleAdmin)
	do := func(method, path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.Header.Set("Authorization", admin)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// First object claims the token.
	rec := do(http.MethodPost, "/api/v1/staff/objects", `{"name":"First","qr_token":"QR-DUP"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("first create: got %d; body %s", rec.Code, rec.Body)
	}
	// A second object created with the same token -> 409 qr-token-taken.
	rec2 := do(http.MethodPost, "/api/v1/staff/objects", `{"name":"Second","qr_token":"QR-DUP"}`)
	if rec2.Code != http.StatusConflict {
		t.Fatalf("dup create: got %d, want 409; body %s", rec2.Code, rec2.Body)
	}
	if !strings.Contains(rec2.Body.String(), "qr-token-taken") {
		t.Errorf("dup create body must carry the qr-token-taken slug; body %s", rec2.Body)
	}

	// A third, distinct object patched to steal the first object's token -> 409.
	rec3 := do(http.MethodPost, "/api/v1/staff/objects", `{"name":"Third","qr_token":"QR-OWN"}`)
	if rec3.Code != http.StatusCreated {
		t.Fatalf("third create: got %d; body %s", rec3.Code, rec3.Body)
	}
	thirdID := extractID(t, rec3.Body.String())
	recPatch := do(http.MethodPatch, "/api/v1/staff/objects/"+thirdID, `{"qr_token":"QR-DUP"}`)
	if recPatch.Code != http.StatusConflict {
		t.Fatalf("dup patch: got %d, want 409; body %s", recPatch.Code, recPatch.Body)
	}
	if !strings.Contains(recPatch.Body.String(), "qr-token-taken") {
		t.Errorf("dup patch body must carry the qr-token-taken slug; body %s", recPatch.Body)
	}
}

// extractID pulls the top-level "id" from a small JSON body.
func extractID(t *testing.T, body string) string {
	t.Helper()
	i := strings.Index(body, `"id":"`)
	if i < 0 {
		t.Fatalf("no id in %s", body)
	}
	rest := body[i+6:]
	return rest[:strings.IndexByte(rest, '"')]
}

func TestStaffObjectExecutionsCursorPagination(t *testing.T) {
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
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'O')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,current_date)`, order, tenant, object, version, worker)
	// 5 executions with strictly increasing created_at. $5 is passed as a
	// string (not int): the query infers its placeholder type as text via the
	// "||" concatenation, and pgx v5 has no encode plan for a bare Go int
	// against a text OID.
	for i := 0; i < 5; i++ {
		must(`INSERT INTO work_execution (id,tenant_id,work_order_id,worker_id,created_at) VALUES ($1,$2,$3,$4, now() + ($5||' seconds')::interval)`,
			uuid.New(), tenant, order, worker, strconv.Itoa(i))
	}
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	admin := bearerAs(t, tenant, uuid.New(), auth.RoleAdmin)
	get := func(path string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := get("/api/v1/staff/objects/" + object.String() + "/executions?limit=2")
	if rec.Code != http.StatusOK {
		t.Fatalf("page1: got %d; body %s", rec.Code, rec.Body)
	}
	if n := strings.Count(rec.Body.String(), `"work_order_id"`); n != 2 {
		t.Fatalf("page1 size: got %d, want 2", n)
	}
	cur := extractJSON(t, rec.Body.String(), "next_cursor")
	rec2 := get("/api/v1/staff/objects/" + object.String() + "/executions?limit=2&cursor=" + cur)
	if rec2.Code != http.StatusOK || strings.Count(rec2.Body.String(), `"work_order_id"`) != 2 {
		t.Fatalf("page2: got %d; body %s", rec2.Code, rec2.Body)
	}
	if rec.Body.String() == rec2.Body.String() {
		t.Error("page2 must differ from page1")
	}
	// Bad cursor -> 422 invalid-cursor.
	if bad := get("/api/v1/staff/objects/" + object.String() + "/executions?cursor=%2Bgarbage"); bad.Code != http.StatusUnprocessableEntity {
		t.Errorf("bad cursor: got %d, want 422", bad.Code)
	}
}

// extractJSON pulls a top-level string field out of a small JSON object.
func extractJSON(t *testing.T, body, key string) string {
	t.Helper()
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("key %q not in %s", key, body)
	}
	rest := body[i+len(needle):]
	return rest[:strings.IndexByte(rest, '"')]
}
