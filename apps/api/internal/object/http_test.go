package object_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

func TestListObjectsIsTenantScoped(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	var tenantA, tenantB uuid.UUID
	for _, row := range []struct {
		id   *uuid.UUID
		name string
	}{{&tenantA, "A"}, {&tenantB, "B"}} {
		*row.id = uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO tenant (id, name) VALUES ($1, $2)`, *row.id, row.name); err != nil {
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

	cfg := config.Config{DevTenantHeaderAuth: true}
	router, _ := app.New(cfg, app.Deps{Pool: pool})

	get := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// No auth header → 401 problem+json.
	if rec := get(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401; body: %s", rec.Code, rec.Body)
	}

	// Tenant A sees exactly its one active object.
	rec := get(map[string]string{"X-Dev-Tenant-Id": tenantA.String()})
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

func TestDevAuthDisabledMeansNoAccess(t *testing.T) {
	pool := testdb.New(t)
	router, _ := app.New(config.Config{DevTenantHeaderAuth: false}, app.Deps{Pool: pool})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
	req.Header.Set("X-Dev-Tenant-Id", uuid.NewString())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dev auth off: got %d, want 401", rec.Code)
	}
}
