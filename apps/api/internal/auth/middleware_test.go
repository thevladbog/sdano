package auth_test

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/testdb"
)

// buildAPI wires an authenticated test API with one GET (read) and one POST
// (mutation) staff op, both protected by the real middleware. It returns the
// pool so tests can seed tenants directly.
func buildAPI(t *testing.T) (humatest.TestAPI, *pgxpool.Pool) {
	t.Helper()
	pool := testdb.New(t)
	_, api := humatest.New(t)
	a := auth.NewAuthenticator(testSecret, db.New(pool))
	api.UseMiddleware(a.Authenticate, a.Authorize)

	huma.Register(api, huma.Operation{
		OperationID: "staffRead", Method: http.MethodGet, Path: "/api/v1/staff/thing",
	}, func(context.Context, *struct{}) (*struct{ Body struct{ OK bool `json:"ok"` } }, error) {
		out := &struct{ Body struct{ OK bool `json:"ok"` } }{}
		out.Body.OK = true
		return out, nil
	})
	huma.Register(api, huma.Operation{
		OperationID: "staffWrite", Method: http.MethodPost, Path: "/api/v1/staff/thing",
	}, func(context.Context, *struct{ Body struct{} }) (*struct{ Body struct{ OK bool `json:"ok"` } }, error) {
		out := &struct{ Body struct{ OK bool `json:"ok"` } }{}
		out.Body.OK = true
		return out, nil
	})
	return api, pool
}

func seedTenant(t *testing.T, pool *pgxpool.Pool, status string) uuid.UUID {
	t.Helper()
	id := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO tenant (id, name, status) VALUES ($1, 'T', $2::tenant_status)`, id, status); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	return id
}

func staffToken(t *testing.T, tenant uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: uuid.New(), TenantID: tenant, Role: role}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return tok
}

func TestAuthenticateRejectsMissingAndBadToken(t *testing.T) {
	api, _ := buildAPI(t)
	if resp := api.Get("/api/v1/staff/thing"); resp.Code != http.StatusUnauthorized {
		t.Errorf("no token: got %d, want 401", resp.Code)
	} else {
		if ct := resp.Header().Get("Content-Type"); ct != "application/problem+json" {
			t.Errorf("error Content-Type = %q, want application/problem+json", ct)
		}
		if !strings.Contains(resp.Body.String(), "authentication-required") {
			t.Errorf("error body must carry the stable slug; body: %s", resp.Body)
		}
	}
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer garbage"); resp.Code != http.StatusUnauthorized {
		t.Errorf("garbage token: got %d, want 401", resp.Code)
	}
}

func TestAuthorizeStaffHappyPathAndRoleGate(t *testing.T) {
	api, pool := buildAPI(t)
	tenant := seedTenant(t, pool, "active")

	admin := staffToken(t, tenant, auth.RoleAdmin)
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer "+admin); resp.Code != http.StatusOK {
		t.Errorf("admin read: got %d, want 200; body %s", resp.Code, resp.Body)
	}
	// A worker token authenticates but is forbidden on a staff route (403, not 401).
	workerTok := staffToken(t, tenant, auth.RoleWorker)
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer "+workerTok); resp.Code != http.StatusForbidden {
		t.Errorf("worker on staff route: got %d, want 403", resp.Code)
	}
}

func TestTenantStatusGate(t *testing.T) {
	api, pool := buildAPI(t)

	archived := staffToken(t, seedTenant(t, pool, "archived"), auth.RoleAdmin)
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer "+archived); resp.Code != http.StatusUnauthorized {
		t.Errorf("archived tenant: got %d, want 401", resp.Code)
	}

	suspTenant := seedTenant(t, pool, "suspended")
	susp := staffToken(t, suspTenant, auth.RoleAdmin)
	// Reads allowed under suspension.
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer "+susp); resp.Code != http.StatusOK {
		t.Errorf("suspended read: got %d, want 200", resp.Code)
	}
	// Mutations rejected 403 tenant-suspended.
	if resp := api.Post("/api/v1/staff/thing", "Authorization: Bearer "+susp, map[string]any{}); resp.Code != http.StatusForbidden {
		t.Errorf("suspended write: got %d, want 403; body %s", resp.Code, resp.Body)
	}
}
