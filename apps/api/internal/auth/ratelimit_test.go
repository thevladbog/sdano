package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"sdano.app/api/internal/auth"
)

// okHandler is a trivial 200 handler for test operations.
func okHandler(context.Context, *struct{}) (*struct {
	Body struct {
		OK bool `json:"ok"`
	}
}, error) {
	out := &struct {
		Body struct {
			OK bool `json:"ok"`
		}
	}{}
	out.Body.OK = true
	return out, nil
}

func TestLimitByIPAuthClassBurst(t *testing.T) {
	_, api := humatest.New(t)
	rl := auth.NewRateLimiter(auth.RateLimitConfig{AuthPerMin: 1, HealthzPerMin: 1, IPCeilingPerMin: 1, PrincipalPerMin: 1})
	api.UseMiddleware(rl.LimitByIP)
	huma.Register(api, huma.Operation{
		OperationID: "authThing", Method: http.MethodGet, Path: "/api/v1/auth/thing", Metadata: auth.Public(),
	}, okHandler)

	if resp := api.Get("/api/v1/auth/thing"); resp.Code != http.StatusOK {
		t.Fatalf("first auth request: got %d, want 200", resp.Code)
	}
	if resp := api.Get("/api/v1/auth/thing"); resp.Code != http.StatusTooManyRequests {
		t.Errorf("second auth request: got %d, want 429", resp.Code)
	}
}

// TestLimitByIPTokenRotationSharesIPBucket is the bypass regression: the limiter
// runs before authentication and keys on IP, so rotating bearer tokens no longer
// mints a fresh bucket per token.
func TestLimitByIPTokenRotationSharesIPBucket(t *testing.T) {
	_, api := humatest.New(t)
	rl := auth.NewRateLimiter(auth.RateLimitConfig{AuthPerMin: 100, HealthzPerMin: 100, IPCeilingPerMin: 2, PrincipalPerMin: 100})
	api.UseMiddleware(rl.LimitByIP)
	huma.Register(api, huma.Operation{
		OperationID: "staffThing", Method: http.MethodGet, Path: "/api/v1/staff/thing",
	}, okHandler)

	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer token-one"); resp.Code != http.StatusOK {
		t.Fatalf("request 1 (token-one): got %d, want 200", resp.Code)
	}
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer token-two"); resp.Code != http.StatusOK {
		t.Fatalf("request 2 (token-two): got %d, want 200", resp.Code)
	}
	if resp := api.Get("/api/v1/staff/thing", "Authorization: Bearer token-three"); resp.Code != http.StatusTooManyRequests {
		t.Errorf("request 3 (token-three) must be refused on the shared IP bucket: got %d, want 429", resp.Code)
	}
}

// TestLimitByIPHealthzClassIsolated proves /healthz has its own class and is not
// starved by an auth-endpoint flood.
func TestLimitByIPHealthzClassIsolated(t *testing.T) {
	_, api := humatest.New(t)
	rl := auth.NewRateLimiter(auth.RateLimitConfig{AuthPerMin: 1, HealthzPerMin: 1, IPCeilingPerMin: 1, PrincipalPerMin: 1})
	api.UseMiddleware(rl.LimitByIP)
	huma.Register(api, huma.Operation{
		OperationID: "authThing", Method: http.MethodGet, Path: "/api/v1/auth/thing", Metadata: auth.Public(),
	}, okHandler)
	huma.Register(api, huma.Operation{
		OperationID: "healthz", Method: http.MethodGet, Path: "/healthz", Metadata: auth.Public(),
	}, okHandler)

	_ = api.Get("/api/v1/auth/thing") // consume the single auth token
	if resp := api.Get("/api/v1/auth/thing"); resp.Code != http.StatusTooManyRequests {
		t.Fatalf("auth class should be exhausted: got %d, want 429", resp.Code)
	}
	if resp := api.Get("/healthz"); resp.Code != http.StatusOK {
		t.Errorf("healthz must not be starved by the auth flood: got %d, want 200", resp.Code)
	}
}
