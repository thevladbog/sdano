package auth

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/google/uuid"
)

// TestLimitByPrincipalIsPerPrincipal verifies the post-auth tier keys on the
// verified principal, so two principals behind one IP get independent buckets.
func TestLimitByPrincipalIsPerPrincipal(t *testing.T) {
	_, api := humatest.New(t)
	rl := NewRateLimiter(RateLimitConfig{AuthPerMin: 100, HealthzPerMin: 100, IPCeilingPerMin: 100, PrincipalPerMin: 1})

	pA := Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: RoleWorker}
	pB := Principal{UserID: uuid.New(), TenantID: uuid.New(), Role: RoleWorker}
	// inject stands in for Authenticate: it attaches a principal chosen by header.
	inject := func(ctx huma.Context, next func(huma.Context)) {
		p := pA
		if ctx.Header("X-Test-Principal") == "B" {
			p = pB
		}
		next(huma.WithContext(ctx, withPrincipal(ctx.Context(), p)))
	}
	api.UseMiddleware(inject, rl.LimitByPrincipal)
	huma.Register(api, huma.Operation{
		OperationID: "staffThing", Method: http.MethodGet, Path: "/api/v1/staff/thing",
	}, func(context.Context, *struct{}) (*struct {
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
	})

	if resp := api.Get("/api/v1/staff/thing", "X-Test-Principal: A"); resp.Code != http.StatusOK {
		t.Fatalf("principal A request 1: got %d, want 200", resp.Code)
	}
	if resp := api.Get("/api/v1/staff/thing", "X-Test-Principal: A"); resp.Code != http.StatusTooManyRequests {
		t.Errorf("principal A request 2: got %d, want 429", resp.Code)
	}
	if resp := api.Get("/api/v1/staff/thing", "X-Test-Principal: B"); resp.Code != http.StatusOK {
		t.Errorf("principal B must be independent of A: got %d, want 200", resp.Code)
	}
}

// TestSweepEvictsIdleButKeepsRecentBuckets drives the lazy TTL eviction with a
// fake clock: an idle bucket is evicted on the next bucket creation, a
// recently-used bucket and the new bucket are retained.
func TestSweepEvictsIdleButKeepsRecentBuckets(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{AuthPerMin: 10, HealthzPerMin: 10, IPCeilingPerMin: 10, PrincipalPerMin: 10})
	base := time.Unix(1_000_000, 0)
	cur := base
	rl.now = func() time.Time { return cur }

	rl.allow("g:1.1.1.1", rl.ipCeiling) // created at base
	rl.allow("g:2.2.2.2", rl.ipCeiling) // created at base

	cur = base.Add(idleBucketTTL + time.Minute)
	rl.allow("g:2.2.2.2", rl.ipCeiling) // refreshes lastSeen → recent
	rl.allow("g:3.3.3.3", rl.ipCeiling) // creation triggers the sweep

	rl.mu.Lock()
	_, has1 := rl.buckets["g:1.1.1.1"]
	_, has2 := rl.buckets["g:2.2.2.2"]
	_, has3 := rl.buckets["g:3.3.3.3"]
	rl.mu.Unlock()

	if has1 {
		t.Error("idle bucket 1.1.1.1 should have been evicted")
	}
	if !has2 {
		t.Error("recently-used bucket 2.2.2.2 should be retained")
	}
	if !has3 {
		t.Error("newly-created bucket 3.3.3.3 should be present")
	}
}

// TestAllowFailsOpenAtMaxBuckets verifies the hard cap: once the bucket map is
// full of active buckets that sweep cannot reclaim, a new key is admitted (fails
// open) without growing the map past the cap.
func TestAllowFailsOpenAtMaxBuckets(t *testing.T) {
	rl := NewRateLimiter(RateLimitConfig{AuthPerMin: 1, HealthzPerMin: 1, IPCeilingPerMin: 1, PrincipalPerMin: 1})
	base := time.Unix(1_000_000, 0)
	rl.now = func() time.Time { return base }
	rl.maxBuckets = 2

	// Fill to capacity with two distinct, active keys.
	rl.allow("g:1.1.1.1", rl.ipCeiling)
	rl.allow("g:2.2.2.2", rl.ipCeiling)

	// A third distinct key at capacity must fail open (be admitted) and must not
	// be tracked, so the map never grows past the cap.
	if !rl.allow("g:3.3.3.3", rl.ipCeiling) {
		t.Error("new key at max capacity must fail open (be admitted)")
	}
	rl.mu.Lock()
	n := len(rl.buckets)
	_, has3 := rl.buckets["g:3.3.3.3"]
	rl.mu.Unlock()
	if n > 2 {
		t.Errorf("bucket map grew past cap: len=%d, want <= 2", n)
	}
	if has3 {
		t.Error("failed-open key must not be tracked in the bucket map")
	}
}
