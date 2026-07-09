package auth_test

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

	router, _ := app.New(config.Config{JWTSecret: "test-secret"}, app.Deps{Pool: pool})
	post := func(path, body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

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
