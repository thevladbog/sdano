# Auth Rate-Limiter Production Hardening Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make the auth rate limiter correct under the production topology (Caddy reverse proxy in front of the API): key on the real client IP, close the pre-auth token-keyed bypass, and bound the bucket map.

**Architecture:** Resolve the real client IP from `X-Forwarded-For` via chi's `ClientIPFromXFFTrustedProxies`, gated on a new `TRUSTED_PROXY_COUNT` env var. Replace the single limiter with two tiers: `LimitByIP` runs first (before any DB work), keyed on the real IP with three path-selected classes (isolated `/healthz`, strict `/api/v1/auth/*`, a general DoS ceiling on everything else); `LimitByPrincipal` runs after `Authenticate`, keyed on the verified principal for fair per-worker quota under carrier-grade NAT. The bucket map is bounded by lazy TTL eviction.

**Tech Stack:** Go 1.26, huma v2.38 + chi v5.3.1 (`chi/v5/middleware` — already a dependency), `golang.org/x/time/rate` (already present), humatest + testcontainers-go. **No new dependencies.**

**Spec:** `docs/superpowers/specs/2026-07-09-auth-ratelimit-hardening-design.md`.

## Global Constraints

- **Base branch:** work stacks on `feat/auth`; this branch is already rebased onto `feat/auth` HEAD. The PR targets `feat/auth`.
- **No new dependencies.** `github.com/go-chi/chi/v5/middleware` and `golang.org/x/time/rate` are already in `go.mod`. `govulncheck` and `npm audit` stay green. No `go.mod`/`go.sum` change is expected.
- **No type-pipeline change.** The limiter is internal middleware; it changes no huma handler input/output types, so the OpenAPI spec and orval clients do not change. `make drift` must remain a no-op. Never hand-edit generated code.
- **`tenant_id`:** not applicable — the limiter issues no domain queries.
- **RFC 7807 problem+json:** the `rate-limited` slug already exists and is reused verbatim; do not invent new slugs.
- **Zero `golangci-lint` warnings.** `slog` only (no `fmt.Println`); wrap errors with context at boundaries.
- **Rate-limit budgets are tunable, not contract** (per the auth spec). Values chosen here are code constants.
- **Tests that hit Postgres use testcontainers.** On this machine (Podman backend), any task running a DB-backed `go test` must first export:
  ```bash
  export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')
  export TESTCONTAINERS_RYUK_DISABLED=true
  ```
  The limiter unit tests (Task 2) use `humatest` only and need no DB/podman env. The app-level integration test (Task 3) and the final sweep (Task 5) need it.
- **Conventional commits**, authored as `Vladislav Bogatyrev <vladislav.bogatyrev@gmail.com>`, each ending with a `Co-Authored-By: Claude Opus 4.8 <noreply@anthropic.com>` trailer. Run Go commands from `apps/api/`; run `make` targets from the repo root.
- Module path `sdano.app/api`. Package `auth` lives at `apps/api/internal/auth/`.

---

## Task index

1. Config — `TRUSTED_PROXY_COUNT`
2. Two-tier rate limiter + app wiring
3. App-level trusted-proxy integration test
4. Docs, env, and compose updates
5. Final verification sweep

---

### Task 1: Config — `TRUSTED_PROXY_COUNT`

**Files:**
- Modify: `apps/api/internal/config/config.go`
- Test: `apps/api/internal/config/config_test.go`

**Interfaces:**
- Consumes: the existing `config.Config` struct and `Load(getenv func(string) string) (Config, error)`.
- Produces: `config.Config.TrustedProxyCount int` — number of trusted reverse-proxy hops; `0` (default, when the env var is unset) means the API is directly exposed. Task 2 reads this field in `app.New`.

- [ ] **Step 1: Write the failing tests**

Append to `apps/api/internal/config/config_test.go`:
```go
func TestLoadDefaultsTrustedProxyCountToZero(t *testing.T) {
	cfg, err := Load(fakeEnv(validEnv()))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxyCount != 0 {
		t.Errorf("TrustedProxyCount default = %d, want 0", cfg.TrustedProxyCount)
	}
}

func TestLoadParsesTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "1"
	cfg, err := Load(fakeEnv(env))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.TrustedProxyCount != 1 {
		t.Errorf("TrustedProxyCount = %d, want 1", cfg.TrustedProxyCount)
	}
}

func TestLoadRejectsNegativeTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "-1"
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must reject a negative TRUSTED_PROXY_COUNT")
	}
}

func TestLoadRejectsNonNumericTrustedProxyCount(t *testing.T) {
	env := validEnv()
	env["TRUSTED_PROXY_COUNT"] = "notanumber"
	if _, err := Load(fakeEnv(env)); err == nil {
		t.Fatal("Load must reject a non-numeric TRUSTED_PROXY_COUNT")
	}
}
```

- [ ] **Step 2: Run the tests to verify they fail**

Run: `cd apps/api && go test ./internal/config/ -run TrustedProxy`
Expected: FAIL — `cfg.TrustedProxyCount undefined`.

- [ ] **Step 3: Add the field**

In `apps/api/internal/config/config.go`, add the field to the `Config` struct after `JWTSecret`:
```go
	JWTSecret      string
	// TrustedProxyCount is the number of trusted reverse-proxy hops in front of
	// the API. 0 (default) means the API is directly exposed; the limiter then
	// keys on the TCP peer. Behind Caddy (prod compose profile) set this to 1 so
	// the real client IP is read from X-Forwarded-For.
	TrustedProxyCount int
```

- [ ] **Step 4: Parse and validate it in `Load`**

In `Load`, after the `S3_USE_PATH_STYLE` parse block, add:
```go
	if cfg.TrustedProxyCount, err = parseCount(getenv, "TRUSTED_PROXY_COUNT"); err != nil {
		return Config{}, err
	}
```
Then add this helper next to `parseBool`:
```go
// parseCount reads a non-negative integer env var, defaulting to 0 when unset.
func parseCount(getenv func(string) string, name string) (int, error) {
	raw := getenv(name)
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, errors.Join(fmt.Errorf("parsing %s", name), err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s must be >= 0, got %d", name, n)
	}
	return n, nil
}
```
(`strconv`, `errors`, and `fmt` are already imported.)

- [ ] **Step 5: Run the config tests to verify they pass**

Run: `cd apps/api && go test ./internal/config/`
Expected: PASS (new tests plus the existing ones).

- [ ] **Step 6: Lint + commit**

```bash
cd apps/api && golangci-lint run && cd ..
git add apps/api/internal/config/config.go apps/api/internal/config/config_test.go
git commit -m "feat(auth): add TRUSTED_PROXY_COUNT config for real-client-IP rate limiting"
```

---

### Task 2: Two-tier rate limiter + app wiring

**Files:**
- Modify (full rewrite): `apps/api/internal/auth/ratelimit.go`
- Modify (full rewrite): `apps/api/internal/auth/ratelimit_test.go` (black-box unit tests)
- Create: `apps/api/internal/auth/ratelimit_internal_test.go` (white-box unit tests)
- Modify: `apps/api/internal/app/app.go` (install trusted-proxy middleware; new limiter API; new chain)

**Interfaces:**
- Consumes: `isPublic(*huma.Operation) bool`, `bearer(huma.Context) string`, `writeProblem(huma.Context, int, string, string)`, `PrincipalFrom(context.Context) (Principal, bool)`, `withPrincipal(context.Context, Principal) context.Context`, `Public() map[string]any`, `Principal{UserID, TenantID uuid.UUID; Role Role}`, `RoleWorker` — all already in the `auth` package. `middleware.GetClientIP(context.Context) string` and `middleware.ClientIPFromXFFTrustedProxies(int) func(http.Handler) http.Handler` from `github.com/go-chi/chi/v5/middleware`. `config.Config.TrustedProxyCount` (Task 1).
- Produces (used by app.go and Task 3):
  - `type RateLimitConfig struct { AuthPerMin, HealthzPerMin, IPCeilingPerMin, PrincipalPerMin int }`.
  - `NewRateLimiter(cfg RateLimitConfig) *RateLimiter`.
  - `(*RateLimiter).LimitByIP(ctx huma.Context, next func(huma.Context))` — pre-auth tier; install first.
  - `(*RateLimiter).LimitByPrincipal(ctx huma.Context, next func(huma.Context))` — post-`Authenticate` tier.

- [ ] **Step 1: Write the black-box unit tests (replace `ratelimit_test.go`)**

Replace the entire contents of `apps/api/internal/auth/ratelimit_test.go` with:
```go
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
```

- [ ] **Step 2: Write the white-box unit tests (new `ratelimit_internal_test.go`)**

Create `apps/api/internal/auth/ratelimit_internal_test.go`:
```go
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
```

- [ ] **Step 3: Run the tests to verify they fail**

Run: `cd apps/api && go test ./internal/auth/ -run 'LimitBy|Sweep'`
Expected: FAIL to compile — `undefined: auth.RateLimitConfig`, `undefined: rl.LimitByIP`, `rl.now undefined`, etc.

- [ ] **Step 4: Rewrite `ratelimit.go`**

Replace the entire contents of `apps/api/internal/auth/ratelimit.go` with:
```go
package auth

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/go-chi/chi/v5/middleware"
	"golang.org/x/time/rate"
)

// RateLimitConfig carries the per-minute request budgets for the limiter's four
// classes. Budgets are tunable, not contract (see the auth spec).
type RateLimitConfig struct {
	AuthPerMin      int // /api/v1/auth/* per client IP (brute-force target)
	HealthzPerMin   int // /healthz per client IP (isolated from the other classes)
	IPCeilingPerMin int // every other route, per client IP, pre-auth (a DoS backstop)
	PrincipalPerMin int // authenticated ops, per verified principal (fair per-worker quota)
}

// RateLimiter throttles requests in two tiers. LimitByIP runs first, before
// authentication, keyed on the real client IP (resolved from the trusted proxy
// — see clientIP): a strict class for /api/v1/auth/*, an isolated class for
// /healthz, and a generous ceiling on everything else. LimitByPrincipal runs
// after authentication, keyed on the verified principal, giving each worker
// fair quota even when many share one carrier-grade-NAT IP.
//
// Buckets live in a mutex-guarded map bounded by lazy TTL eviction. Keys are
// only IP strings and principal UUIDs — never bearer tokens — so no secret
// material is ever retained.
type RateLimiter struct {
	auth      limit
	healthz   limit
	ipCeiling limit
	principal limit

	mu      sync.Mutex
	buckets map[string]*bucket
	// now is the clock, overridable in tests for deterministic eviction.
	now       func() time.Time
	lastSweep time.Time
}

// limit is a rate/burst pair derived from a per-minute budget.
type limit struct {
	rate  rate.Limit
	burst int
}

// bucket is a token-bucket limiter tagged with its last-access time for eviction.
type bucket struct {
	lim      *rate.Limiter
	lastSeen time.Time
}

const (
	// idleBucketTTL is how long a bucket may sit unused before eviction. It is
	// far longer than any refill window (the strictest class refills within a
	// minute), so evicting an idle bucket never discards live throttle state —
	// an idle bucket would have refilled to full anyway.
	idleBucketTTL = 15 * time.Minute
	// sweepInterval bounds how often we scan the map for idle buckets.
	sweepInterval = 5 * time.Minute
	// maxBuckets forces an out-of-schedule sweep if cardinality spikes.
	maxBuckets = 100_000

	healthzPath = "/healthz"
	authPrefix  = "/api/v1/auth/"
)

// NewRateLimiter builds a limiter from per-minute request budgets.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		auth:      newLimit(cfg.AuthPerMin),
		healthz:   newLimit(cfg.HealthzPerMin),
		ipCeiling: newLimit(cfg.IPCeilingPerMin),
		principal: newLimit(cfg.PrincipalPerMin),
		buckets:   make(map[string]*bucket),
		now:       time.Now,
	}
}

// newLimit converts a per-minute budget into a rate/burst pair: the burst is the
// whole per-minute allowance, refilled steadily over the minute.
func newLimit(perMin int) limit {
	return limit{rate: rate.Limit(float64(perMin) / 60.0), burst: perMin}
}

// LimitByIP is the pre-authentication tier. It keys every request on the real
// client IP and selects the class by path. It must run first in the chain, before
// Authenticate, so a flood is refused before it can cost a database lookup.
func (rl *RateLimiter) LimitByIP(ctx huma.Context, next func(huma.Context)) {
	ip := clientIP(ctx)
	var key string
	var l limit
	switch path := ctx.Operation().Path; {
	case path == healthzPath:
		key, l = "h:"+ip, rl.healthz
	case strings.HasPrefix(path, authPrefix):
		key, l = "a:"+ip, rl.auth
	default:
		key, l = "g:"+ip, rl.ipCeiling
	}
	if !rl.allow(key, l) {
		writeProblem(ctx, http.StatusTooManyRequests, "rate-limited", "too many requests")
		return
	}
	next(ctx)
}

// LimitByPrincipal is the post-authentication tier. It keys on the verified
// principal so shared-IP workers each get fair quota. Public operations (which
// carry no principal) pass through — they are already covered by LimitByIP.
func (rl *RateLimiter) LimitByPrincipal(ctx huma.Context, next func(huma.Context)) {
	if isPublic(ctx.Operation()) {
		next(ctx)
		return
	}
	p, ok := PrincipalFrom(ctx.Context())
	if !ok {
		// Authenticate rejects non-public requests without a principal, so this
		// is defensive: never throttle what we cannot attribute.
		next(ctx)
		return
	}
	if !rl.allow("p:"+p.TenantID.String()+":"+p.UserID.String(), rl.principal) {
		writeProblem(ctx, http.StatusTooManyRequests, "rate-limited", "too many requests")
		return
	}
	next(ctx)
}

// allow reports whether the bucket for key admits one more request, creating the
// bucket on first use and refreshing its last-seen time.
func (rl *RateLimiter) allow(key string, l limit) bool {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	now := rl.now()
	b, ok := rl.buckets[key]
	if !ok {
		rl.sweep(now)
		b = &bucket{lim: rate.NewLimiter(l.rate, l.burst)}
		rl.buckets[key] = b
	}
	b.lastSeen = now
	return b.lim.Allow()
}

// sweep evicts idle buckets. It runs on bucket creation but only does work once
// per sweepInterval or when the map exceeds maxBuckets, so the common path stays
// cheap. The caller holds rl.mu.
func (rl *RateLimiter) sweep(now time.Time) {
	if len(rl.buckets) < maxBuckets && now.Sub(rl.lastSweep) < sweepInterval {
		return
	}
	for k, b := range rl.buckets {
		if now.Sub(b.lastSeen) > idleBucketTTL {
			delete(rl.buckets, k)
		}
	}
	rl.lastSweep = now
}

// clientIP returns the real client IP for rate-limit keying. Behind a trusted
// reverse proxy (TRUSTED_PROXY_COUNT > 0) app.New installs chi's
// ClientIPFromXFFTrustedProxies, which resolves the client from X-Forwarded-For
// into a request-context value; we read it here. With no trusted proxy
// configured (dev, tests, or a request that reached us without the header) we
// fall back to the direct TCP peer.
func clientIP(ctx huma.Context) string {
	if ip := middleware.GetClientIP(ctx.Context()); ip != "" {
		return ip
	}
	if host, _, err := net.SplitHostPort(ctx.RemoteAddr()); err == nil {
		return host
	}
	return ctx.RemoteAddr()
}
```

- [ ] **Step 5: Rewire `app.go`**

In `apps/api/internal/app/app.go`, replace the `RealIP` comment block (immediately after `router.Use(middleware.RequestID)`) with the conditional trusted-proxy middleware:
```go
	router := chi.NewMux()
	router.Use(middleware.RequestID)
	// Behind a reverse proxy (the prod compose profile puts Caddy in front of
	// the API — see deploy/Caddyfile), the direct TCP peer is always the proxy,
	// so per-IP rate limiting would collapse to a single bucket. When
	// TRUSTED_PROXY_COUNT > 0 we resolve the real client from X-Forwarded-For via
	// chi's trusted-proxy middleware (it stores the IP in a context value the
	// rate limiter reads). middleware.RealIP is intentionally NOT used — it is
	// deprecated upstream as IP-spoofable (GHSA-3fxj-6jh8-hvhx).
	if cfg.TrustedProxyCount > 0 {
		router.Use(middleware.ClientIPFromXFFTrustedProxies(cfg.TrustedProxyCount))
	}
```
Then replace the limiter construction and middleware-install lines:
```go
	authn := auth.NewAuthenticator(cfg.JWTSecret, queries)
	rl := auth.NewRateLimiter(auth.RateLimitConfig{
		AuthPerMin:      10,   // /api/v1/auth/* per client IP
		HealthzPerMin:   60,   // /healthz per client IP (isolated)
		IPCeilingPerMin: 1200, // pre-auth DoS ceiling on all other routes, per client IP
		PrincipalPerMin: 300,  // authenticated ops, per verified principal
	})
	api.UseMiddleware(rl.LimitByIP, authn.Authenticate, rl.LimitByPrincipal, authn.Authorize)
```
(`github.com/go-chi/chi/v5/middleware` is already imported for `RequestID`; no import change.)

- [ ] **Step 6: Run the auth + app unit tests to verify they pass**

Run: `cd apps/api && go build ./... && go test ./internal/auth/ -run 'LimitBy|Sweep'`
Expected: build clean; the limiter unit tests PASS (no DB needed).

- [ ] **Step 7: Lint the full module**

Run: `cd apps/api && golangci-lint run`
Expected: zero warnings.

- [ ] **Step 8: Commit**

```bash
git add apps/api/internal/auth/ratelimit.go apps/api/internal/auth/ratelimit_test.go \
        apps/api/internal/auth/ratelimit_internal_test.go apps/api/internal/app/app.go
git commit -m "feat(auth): two-tier rate limiter keyed on real client IP and principal"
```

---

### Task 3: App-level trusted-proxy integration test

**Files:**
- Create: `apps/api/internal/app/ratelimit_test.go` (package `app_test`)

**Interfaces:**
- Consumes: `app.New(config.Config, app.Deps) (*chi.Mux, huma.API)`; `config.Config.TrustedProxyCount` (Task 1); `testdb.New(t)`; the `/api/v1/auth/login` route registered by `app.New`. The auth class budget wired in Task 2 is 10/min.
- Produces: end-to-end proof that behind a trusted proxy the limiter keys on the `X-Forwarded-For` client IP (fixing the collapse), and that without a trusted proxy `X-Forwarded-For` is ignored.

- [ ] **Step 1: Set the Podman testcontainers env**

```bash
export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')
export TESTCONTAINERS_RYUK_DISABLED=true
```

- [ ] **Step 2: Write the integration test**

Create `apps/api/internal/app/ratelimit_test.go`:
```go
package app_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

// loginPoster returns a helper that POSTs to /api/v1/auth/login with the given
// X-Forwarded-For header and returns the status code. Under-budget requests get
// 401 (no such user); the limiter runs before the handler, so an exhausted
// bucket returns 429 regardless.
func loginPoster(t *testing.T, router http.Handler) func(xff string) int {
	t.Helper()
	return func(xff string) int {
		body := []byte(`{"email":"nobody@example.test","password":"whatever-12345"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", xff)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}
}

// TestRateLimitUsesTrustedProxyClientIP proves that with one trusted proxy, two
// different X-Forwarded-For client IPs (same TCP peer — the proxy) get separate
// per-IP buckets, so one client's flood cannot rate-limit another.
func TestRateLimitUsesTrustedProxyClientIP(t *testing.T) {
	pool := testdb.New(t)
	router, _ := app.New(config.Config{TrustedProxyCount: 1}, app.Deps{Pool: pool})
	post := loginPoster(t, router)

	// Exhaust client A's per-IP auth bucket (10/min).
	for i := 1; i <= 10; i++ {
		if code := post("203.0.113.10"); code == http.StatusTooManyRequests {
			t.Fatalf("client A request %d was rate-limited too early", i)
		}
	}
	if code := post("203.0.113.10"); code != http.StatusTooManyRequests {
		t.Fatalf("client A 11th request: got %d, want 429", code)
	}
	// A different client IP is unaffected — the key came from X-Forwarded-For,
	// not the shared proxy RemoteAddr.
	if code := post("203.0.113.20"); code == http.StatusTooManyRequests {
		t.Fatalf("client B was rate-limited by client A's flood — per-IP keying collapsed")
	}
}

// TestRateLimitIgnoresXFFWithoutTrustedProxy proves that with no trusted proxy,
// X-Forwarded-For is ignored: both "clients" share the TCP-peer bucket.
func TestRateLimitIgnoresXFFWithoutTrustedProxy(t *testing.T) {
	pool := testdb.New(t)
	router, _ := app.New(config.Config{TrustedProxyCount: 0}, app.Deps{Pool: pool})
	post := loginPoster(t, router)

	for i := 1; i <= 10; i++ {
		if code := post("203.0.113.10"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate-limited too early", i)
		}
	}
	// XFF is ignored, so a "different" client on the same TCP peer is still limited.
	if code := post("203.0.113.20"); code != http.StatusTooManyRequests {
		t.Fatalf("without a trusted proxy, XFF must be ignored: got %d, want 429", code)
	}
}
```

- [ ] **Step 3: Run the integration test to verify it passes**

Run: `cd apps/api && go test ./internal/app/ -run RateLimit -v`
Expected: PASS (first run pulls the postgres image). Both tests green.

- [ ] **Step 4: Commit**

```bash
git add apps/api/internal/app/ratelimit_test.go
git commit -m "test(auth): app-level proof rate limiting keys on the trusted-proxy client IP"
```

---

### Task 4: Docs, env, and compose updates

**Files:**
- Modify: `.env.example`
- Modify: `deploy/docker-compose.yml`
- Modify: `docs/10-deployment.md`
- Modify: `docs/07-api-spec.md`

**Interfaces:**
- Consumes: `TRUSTED_PROXY_COUNT` (Task 1). No code; keeps operator docs/config in sync with behavior (AGENTS.md: docs update in the same PR as the behavior change).

- [ ] **Step 1: Add `TRUSTED_PROXY_COUNT` to `.env.example`**

In `.env.example`, after the `# --- Auth ---` block (the `JWT_SECRET=` line), add:
```bash

# --- Reverse proxy ---
# Number of trusted reverse-proxy hops in front of the API. Used to resolve the
# real client IP from X-Forwarded-For for rate limiting. 0 (default) = the API is
# directly exposed (key on the TCP peer). The prod compose profile puts Caddy in
# front, so production sets this to 1.
TRUSTED_PROXY_COUNT=0
```

- [ ] **Step 2: Plumb it into the compose api service**

In `deploy/docker-compose.yml`, in the `api` service `environment:` block, after the `JWT_SECRET: ${JWT_SECRET}` line, add:
```yaml
      TRUSTED_PROXY_COUNT: ${TRUSTED_PROXY_COUNT:-0}
```
(The `:-0` default keeps `docker compose` from warning when the var is absent from `.env`.)

- [ ] **Step 3: Update `docs/10-deployment.md` (Security posture)**

In `docs/10-deployment.md`, in the "Security posture" section, replace the bullet:
```
- Caddy: TLS 1.2+, HSTS. API: strict CORS (admin origin), rate limits on auth endpoints.
```
with:
```
- Caddy: TLS 1.2+, HSTS. API: strict CORS (admin origin). Rate limiting is app-level and two-tier: a pre-auth per-client-IP shield (strict on `/api/v1/auth/*`, an isolated class for `/healthz`, a generous ceiling elsewhere) plus a per-principal tier for authenticated traffic. The real client IP is read from `X-Forwarded-For` via one trusted proxy hop — set `TRUSTED_PROXY_COUNT=1` in the prod `.env` so Caddy's address does not become one global bucket.
```

- [ ] **Step 4: Update `docs/07-api-spec.md` (rate limiting)**

In `docs/07-api-spec.md`, replace the bullet:
```
- **Rate limiting:** per-token, generous for workers (photo bursts are legitimate), stricter for auth endpoints.
```
with:
```
- **Rate limiting:** two-tier. Pre-auth, per real client IP (resolved from `X-Forwarded-For` behind the trusted proxy): strict on `/api/v1/auth/*`, an isolated class for `/healthz`, a generous DoS ceiling elsewhere. Post-auth, per verified principal — generous for workers (photo bursts are legitimate). Over-budget requests get `429` with the `rate-limited` problem type. Budgets are tunable, not contract.
```

- [ ] **Step 5: Commit**

```bash
git add .env.example deploy/docker-compose.yml docs/10-deployment.md docs/07-api-spec.md
git commit -m "docs(auth): document TRUSTED_PROXY_COUNT and the two-tier rate-limit model"
```

---

### Task 5: Final verification sweep

**Files:** none new — verification only.

- [ ] **Step 1: Set the Podman testcontainers env**

```bash
export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')
export TESTCONTAINERS_RYUK_DISABLED=true
```

- [ ] **Step 2: Build, vet, lint, full test**

```bash
cd apps/api && go build ./... && go vet ./... && golangci-lint run && go test ./... && cd ..
```
Expected: clean, all green — including `internal/auth`, `internal/app`, `internal/config`, `internal/object`.

- [ ] **Step 3: Confirm no dependency or type-pipeline drift**

```bash
git diff --stat feat/auth -- apps/api/go.mod apps/api/go.sum   # expect: no output (deps unchanged)
make drift                                                     # expect: exit 0, no client regeneration needed
govulncheck ./apps/api/...                                     # expect: no findings
```
Expected: `go.mod`/`go.sum` unchanged; `make drift` clean (the limiter changed no huma types); govulncheck green.

- [ ] **Step 4: Confirm the middleware chain and tree**

```bash
grep -n "UseMiddleware" apps/api/internal/app/app.go   # expect: rl.LimitByIP, authn.Authenticate, rl.LimitByPrincipal, authn.Authorize
git status --short                                      # expect: empty
git log --oneline feat/auth..HEAD                       # expect: the five hardening commits, one concern each
```

- [ ] **Step 5: Report completion**

The limiter now keys on the real client IP behind the trusted proxy (no global-bucket collapse), refuses rotating-token floods before they reach the database (no pre-auth bypass, no plaintext tokens retained), isolates `/healthz`, gives authenticated workers per-principal fairness under carrier-grade NAT, and bounds the bucket map by lazy TTL eviction. Behavior, `.env.example`, compose, and the deployment/API docs are in sync.

## Plan Self-Review (done at write time)

- **Spec coverage:** real client IP via `ClientIPFromXFFTrustedProxies` + `GetClientIP` ✓ (T2, config in T1); `TRUSTED_PROXY_COUNT` default 0 / prod 1 ✓ (T1, T4); two-tier `LimitByIP → Authenticate → LimitByPrincipal → Authorize` ✓ (T2); isolated `/healthz` class ✓ (T2, tested T2 step 1); token-bypass closed (no pre-auth token keying) ✓ (T2, regression test T2 step 1); "hash the key" superseded by never keying on tokens ✓ (noted in spec; keys are IP/UUID only in T2); bounded map via lazy TTL eviction ✓ (T2, tested T2 step 2); unit + app-level XFF integration tests ✓ (T2, T3); docs `.env.example`/compose/`docs/10`/`docs/07` ✓ (T4). Out-of-scope (pre-DB token shape-check, Redis) left unbuilt per spec.
- **Placeholder scan:** none — every step carries exact code or exact commands with expected output.
- **Type consistency:** `RateLimitConfig{AuthPerMin, HealthzPerMin, IPCeilingPerMin, PrincipalPerMin}`, `NewRateLimiter(cfg)`, `LimitByIP`, `LimitByPrincipal`, `allow(key, limit)`, `sweep(now)`, `clientIP(ctx)`, `bucket{lim, lastSeen}`, `rl.now`, `idleBucketTTL`/`sweepInterval`/`maxBuckets`, and `config.Config.TrustedProxyCount` are used identically across Tasks 1–3. The app.go chain matches the method names exactly.
