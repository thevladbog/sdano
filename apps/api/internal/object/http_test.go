package object_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

const testSecret = "test-secret"

func bearer(t *testing.T, tenant uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: uuid.New(), TenantID: tenant, Role: role}, auth.AccessTTL)
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
