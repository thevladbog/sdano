package roster_test

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

const testSecret = "roster-test-secret-at-least-32-bytes!"

func adminDo(t *testing.T, router http.Handler, tenant uuid.UUID, method, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: uuid.New(), TenantID: tenant, Role: auth.RoleAdmin}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	var rdr io.Reader
	if body != "" {
		rdr = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, rdr)
	req.Header.Set("Authorization", "Bearer "+tok)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func extract(t *testing.T, body, key string) string {
	t.Helper()
	needle := `"` + key + `":"`
	i := strings.Index(body, needle)
	if i < 0 {
		t.Fatalf("key %q not in %s", key, body)
	}
	rest := body[i+len(needle):]
	return rest[:strings.IndexByte(rest, '"')]
}

func seedTenant(t *testing.T, pool *pgxpool.Pool) uuid.UUID {
	t.Helper()
	tenant := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	return tenant
}

func TestWorkerLifecycle(t *testing.T) {
	pool := testdb.New(t)
	tenant := seedTenant(t, pool)
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})

	// Create -> 201 with a 6-digit invite code.
	rec := adminDo(t, router, tenant, http.MethodPost, "/api/v1/staff/workers", `{"display_name":"Alexey, crew 2"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: got %d; body %s", rec.Code, rec.Body)
	}
	code := extract(t, rec.Body.String(), "invite_code")
	if len(code) != 6 {
		t.Fatalf("invite code %q must be 6 digits", code)
	}
	workerID := extract(t, rec.Body.String(), "id")

	// The code actually claims (device token issued) — end-to-end through /auth.
	claim := adminDo(t, router, tenant, http.MethodPost, "/api/v1/auth/worker/claim", `{"invite_code":"`+code+`"}`)
	if claim.Code != http.StatusOK {
		t.Fatalf("claim: got %d; body %s", claim.Code, claim.Body)
	}
	devTok := extract(t, claim.Body.String(), "device_token")

	// Reinvite with token revocation: old device token dies, new code claims.
	rec = adminDo(t, router, tenant, http.MethodPost, "/api/v1/staff/workers/"+workerID+"/reinvite", `{"revoke_tokens":true}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("reinvite: got %d; body %s", rec.Code, rec.Body)
	}
	newCode := extract(t, rec.Body.String(), "invite_code")
	if newCode == code {
		t.Error("reinvite must issue a fresh code")
	}
	// Old device token now 401 on a worker route.
	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/today", nil)
	req.Header.Set("Authorization", "Bearer "+devTok)
	r2 := httptest.NewRecorder()
	router.ServeHTTP(r2, req)
	if r2.Code != http.StatusUnauthorized {
		t.Errorf("revoked device token: got %d, want 401", r2.Code)
	}
	// Old invite code voided.
	c2 := adminDo(t, router, tenant, http.MethodPost, "/api/v1/auth/worker/claim", `{"invite_code":"`+code+`"}`)
	if c2.Code != http.StatusUnauthorized {
		t.Errorf("voided code claim: got %d, want 401", c2.Code)
	}

	// Deactivate cuts device auth instantly.
	claim2 := adminDo(t, router, tenant, http.MethodPost, "/api/v1/auth/worker/claim", `{"invite_code":"`+newCode+`"}`)
	if claim2.Code != http.StatusOK {
		t.Fatalf("claim2: got %d", claim2.Code)
	}
	devTok2 := extract(t, claim2.Body.String(), "device_token")
	rec = adminDo(t, router, tenant, http.MethodPatch, "/api/v1/staff/workers/"+workerID, `{"is_active":false}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("deactivate: got %d; body %s", rec.Code, rec.Body)
	}
	req3 := httptest.NewRequest(http.MethodGet, "/api/v1/worker/today", nil)
	req3.Header.Set("Authorization", "Bearer "+devTok2)
	r3 := httptest.NewRecorder()
	router.ServeHTTP(r3, req3)
	if r3.Code != http.StatusUnauthorized {
		t.Errorf("deactivated worker device token: got %d, want 401", r3.Code)
	}

	// List shows the worker (deactivated) — and the pending invite is gone (claimed).
	rec = adminDo(t, router, tenant, http.MethodGet, "/api/v1/staff/workers", "")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), "Alexey, crew 2") {
		t.Fatalf("list: got %d; body %s", rec.Code, rec.Body)
	}
	// Unknown worker patch -> 404.
	if rec = adminDo(t, router, tenant, http.MethodPatch, "/api/v1/staff/workers/"+uuid.NewString(), `{"is_active":true}`); rec.Code != http.StatusNotFound {
		t.Errorf("unknown worker: got %d, want 404", rec.Code)
	}
	// Both bodies are all-optional, so a request without any body must not be
	// rejected by the schema: reinvite defaults to not revoking tokens, and a
	// body-less PATCH is a no-op returning current state (still deactivated).
	if rec = adminDo(t, router, tenant, http.MethodPost, "/api/v1/staff/workers/"+workerID+"/reinvite", ""); rec.Code != http.StatusOK {
		t.Errorf("body-less reinvite: got %d; body %s", rec.Code, rec.Body)
	}
	if rec = adminDo(t, router, tenant, http.MethodPatch, "/api/v1/staff/workers/"+workerID, ""); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"is_active":false`) {
		t.Errorf("body-less patch: got %d; body %s", rec.Code, rec.Body)
	}
}
