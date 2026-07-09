package auth_test

import (
	"context"
	"net/http"
	"testing"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"

	"sdano.app/api/internal/auth"
)

func TestRateLimiterBlocksBurstExcess(t *testing.T) {
	_, api := humatest.New(t)
	// burst = 1: the second immediate request on the same key is refused.
	rl := auth.NewRateLimiter(1, 1)
	api.UseMiddleware(rl.Middleware)
	huma.Register(api, huma.Operation{
		OperationID: "ping", Method: http.MethodGet, Path: "/ping", Metadata: auth.Public(),
	}, func(context.Context, *struct{}) (*struct{ Body struct{ OK bool `json:"ok"` } }, error) {
		out := &struct{ Body struct{ OK bool `json:"ok"` } }{}
		out.Body.OK = true
		return out, nil
	})

	if resp := api.Get("/ping"); resp.Code != http.StatusOK {
		t.Fatalf("first request: got %d, want 200", resp.Code)
	}
	if resp := api.Get("/ping"); resp.Code != http.StatusTooManyRequests {
		t.Errorf("second request: got %d, want 429", resp.Code)
	}
}
