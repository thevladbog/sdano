package auth_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

func TestLoginEndpointRoundTrip(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	hash, _ := auth.HashPassword("password12345")
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (tenant_id, role, display_name, email, password_hash)
		 VALUES ($1, 'admin', 'Boss', 'boss@acme.test', $2)`, tenant, hash); err != nil {
		t.Fatalf("user: %v", err)
	}

	router, post := newTestApp(pool)

	// Bad credentials → 401 with the stable slug.
	if rec := post("/api/v1/auth/login", `{"email":"boss@acme.test","password":"wrong"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid-credentials") {
		t.Fatalf("bad login: got %d body %s", rec.Code, rec.Body)
	}

	// Good credentials → 200 with tokens; the access token opens /staff/objects.
	rec := post("/api/v1/auth/login", `{"email":"boss@acme.test","password":"password12345"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("login: got %d body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "access_token") || !strings.Contains(rec.Body.String(), "refresh_token") {
		t.Fatalf("login body missing tokens: %s", rec.Body)
	}
	access := extractJSONString(t, rec.Body.String(), "access_token")

	req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
	req.Header.Set("Authorization", "Bearer "+access)
	protectedRec := httptest.NewRecorder()
	router.ServeHTTP(protectedRec, req)
	if protectedRec.Code != http.StatusOK {
		t.Fatalf("access token must open /staff/objects: got %d body %s", protectedRec.Code, protectedRec.Body)
	}
}

// newTestApp builds the HTTP router against pool and returns it alongside a
// post helper that sends a JSON body and records the response.
func newTestApp(pool *pgxpool.Pool) (*chi.Mux, func(path, body string) *httptest.ResponseRecorder) {
	router, _ := app.New(config.Config{JWTSecret: "test-secret"}, app.Deps{Pool: pool})
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	return router, post
}

// seedAdminForHTTP inserts a tenant + an active admin with the given
// credentials and returns the tenant id, mirroring the seeding done in
// TestLoginEndpointRoundTrip so the refresh/logout tests can log in for real.
func seedAdminForHTTP(t *testing.T, pool *pgxpool.Pool, email, password string) uuid.UUID {
	t.Helper()
	ctx := context.Background()
	tenant := uuid.New()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (tenant_id, role, display_name, email, password_hash)
		 VALUES ($1, 'admin', 'Boss', $2, $3)`, tenant, email, hash); err != nil {
		t.Fatalf("user: %v", err)
	}
	return tenant
}

func TestRefreshEndpoint(t *testing.T) {
	pool := testdb.New(t)
	seedAdminForHTTP(t, pool, "refresh@acme.test", "password12345")
	_, post := newTestApp(pool)

	loginRec := post("/api/v1/auth/login", `{"email":"refresh@acme.test","password":"password12345"}`)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login: got %d body %s", loginRec.Code, loginRec.Body)
	}
	original := extractJSONString(t, loginRec.Body.String(), "refresh_token")

	// Valid refresh → 200 with a fresh access + refresh token pair.
	rec := post("/api/v1/auth/refresh", `{"refresh_token":"`+original+`"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("refresh: got %d body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "access_token") || !strings.Contains(rec.Body.String(), "refresh_token") {
		t.Fatalf("refresh body missing tokens: %s", rec.Body)
	}
	rotated := extractJSONString(t, rec.Body.String(), "refresh_token")
	if rotated == original {
		t.Errorf("refresh must rotate to a new refresh token, got the same value")
	}

	// Garbage/unknown refresh token → 401 with the stable slug.
	if rec := post("/api/v1/auth/refresh", `{"refresh_token":"garbage"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid-refresh-token") {
		t.Fatalf("bad refresh: got %d body %s", rec.Code, rec.Body)
	}
}

func TestLogoutEndpoint(t *testing.T) {
	pool := testdb.New(t)
	seedAdminForHTTP(t, pool, "logout@acme.test", "password12345")
	_, post := newTestApp(pool)

	loginRec := post("/api/v1/auth/login", `{"email":"logout@acme.test","password":"password12345"}`)
	if loginRec.Code != http.StatusOK {
		t.Fatalf("login: got %d body %s", loginRec.Code, loginRec.Body)
	}
	refreshToken := extractJSONString(t, loginRec.Body.String(), "refresh_token")

	// First logout revokes the token → 204 with an empty body.
	rec := post("/api/v1/auth/logout", `{"refresh_token":"`+refreshToken+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("logout: got %d body %s", rec.Code, rec.Body)
	}
	if rec.Body.Len() != 0 {
		t.Errorf("logout must return an empty body, got %q", rec.Body.String())
	}

	// Logging out an already-revoked token is idempotent → still 204.
	rec = post("/api/v1/auth/logout", `{"refresh_token":"`+refreshToken+`"}`)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("idempotent logout: got %d body %s", rec.Code, rec.Body)
	}

	// The revoked token must no longer work for refresh, proving logout
	// actually revoked it end-to-end through HTTP.
	if rec := post("/api/v1/auth/refresh", `{"refresh_token":"`+refreshToken+`"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invalid-refresh-token") {
		t.Fatalf("refresh after logout: got %d body %s", rec.Code, rec.Body)
	}
}

func TestWorkerClaimEndpoint(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	worker := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1, $2, 'worker', 'Alexey')`,
		worker, tenant); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
		 VALUES ($1, $2, '999111', now() + interval '1 hour')`, tenant, worker); err != nil {
		t.Fatalf("invite: %v", err)
	}
	_, post := newTestApp(pool)

	// Valid, unused invite code → 200 with a device token and the worker.
	rec := post("/api/v1/auth/worker/claim", `{"invite_code":"999111"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("claim: got %d body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), "device_token") || !strings.Contains(rec.Body.String(), "Alexey") {
		t.Fatalf("claim body missing device_token/worker: %s", rec.Body)
	}

	// Reusing the same code → 401 with the stable slug (single-use, proven
	// through HTTP).
	if rec := post("/api/v1/auth/worker/claim", `{"invite_code":"999111"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invite-code-invalid") {
		t.Fatalf("reused code: got %d body %s", rec.Code, rec.Body)
	}

	// Unknown code → 401 with the stable slug.
	if rec := post("/api/v1/auth/worker/claim", `{"invite_code":"000000"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "invite-code-invalid") {
		t.Fatalf("unknown code: got %d body %s", rec.Code, rec.Body)
	}
}

func TestLoginArchivedTenantEndpoint(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := seedAdminForHTTP(t, pool, "boss@arch-http.test", "password12345")
	if _, err := pool.Exec(ctx, `UPDATE tenant SET status = 'archived' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("archive: %v", err)
	}
	_, post := newTestApp(pool)

	// Correct credentials but a dead tenant → 401 with the stable slug, proven
	// end-to-end through HTTP.
	if rec := post("/api/v1/auth/login", `{"email":"boss@arch-http.test","password":"password12345"}`); rec.Code != http.StatusUnauthorized || !strings.Contains(rec.Body.String(), "tenant-archived") {
		t.Fatalf("archived login: got %d body %s", rec.Code, rec.Body)
	}
}

// extractJSONString pulls a top-level string field out of a small JSON object
// without a struct — enough for this test.
func extractJSONString(t *testing.T, body, key string) string {
	t.Helper()
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("key %q not in %s", key, body)
	}
	rest := body[i+len(needle):]
	end := strings.IndexByte(rest, '"')
	if end < 0 {
		t.Fatalf("unterminated value for %q", key)
	}
	return rest[:end]
}
