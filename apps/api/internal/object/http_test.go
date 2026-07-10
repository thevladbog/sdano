package object_test

import (
	"context"
	"net/http"
	"net/http/httptest"
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

	get := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// No token → 401.
	if rec := get(""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no token: got %d, want 401; body: %s", rec.Code, rec.Body)
	}
	// A worker token is authenticated but forbidden on a staff route → 403.
	if rec := get(bearer(t, tenantA, auth.RoleWorker)); rec.Code != http.StatusForbidden {
		t.Fatalf("worker on staff route: got %d, want 403; body: %s", rec.Code, rec.Body)
	}
	// Tenant A admin sees exactly its one active object.
	rec := get(bearer(t, tenantA, auth.RoleAdmin))
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
	if strings.Contains(body, "Retired stop") {
		t.Errorf("inactive objects must be filtered; body: %s", body)
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
