# Sdano — Auth Rate-Limiter Production Hardening Design

**Date:** 2026-07-09
**Status:** approved (brainstorming session with the owner)
**Base branch:** stacks on `feat/auth`; the hardening PR targets `feat/auth` (the auth feature is not yet in `main`).
**Normative context:** builds on `AGENTS.md`, `11-development-rules.md`, `docs/07-api-spec.md`, `docs/10-deployment.md`, and the auth plan `docs/superpowers/plans/2026-07-09-auth.md`. Any contract statement here that extends those docs is merged back into them in the same PR.

## Goal

Make the auth rate limiter (`apps/api/internal/auth/ratelimit.go`) correct under the **production topology** that already ships in the `prod` compose profile (Caddy reverse-proxy in front of the Go API). This is the production-hardening follow-up deferred from the `feat/auth` branch's final review — two topology problems, one hygiene problem.

## Problems (all confirmed against `feat/auth` HEAD)

1. **Per-IP keying collapses to one global bucket behind Caddy.** The limiter is a huma middleware reading `ctx.RemoteAddr()` (→ `r.RemoteAddr`). In the `prod` profile, `deploy/Caddyfile` does `reverse_proxy api:8080`, so every external request's `RemoteAddr` is Caddy's container IP. All public traffic (login/refresh/claim/healthz for the entire internet) shares **one** 10-or-300/min bucket — a trivial DoS, and it trips under normal multi-user load. Sub-issue: `/healthz` shares a bucket class with other traffic, so an uptime monitor and an auth flood can starve each other. The `RealIP`-deprecation comment in `app.go` ("once a reverse proxy is introduced") is stale — the proxy already ships.

2. **Pre-auth token-keyed limiting is bypassable, and each garbage token costs a DB query.** The limiter runs *before* `Authenticate`, keyed on the client-supplied bearer value (`"tok:"+bearer`). An attacker rotating random tokens gets a fresh 300/min bucket per value (effectively unlimited), and each dotless garbage token then reaches `Authenticate`'s `GetDeviceSession` DB query.

3. **The bucket map is unbounded and holds plaintext tokens.** `map[string]*rate.Limiter` never evicts; token-keyed entries retain plaintext bearer values for the process lifetime.

## Design decisions (owner-approved)

- **Enforcement stays app-level (Go), not in Caddy.** Caddy rate limiting needs the third-party `caddy-ratelimit` module + a custom `xcaddy` image (stock `caddy:latest` has none) — a violation of the "boring, single-binary" ethos — and we'd still need the Go limiter for per-principal limits.
- **Two-tier limiting**, because workers run on mobile carriers and many share one carrier-grade-NAT public IP.
- **Trusted-proxy hop count is env-configured** (`TRUSTED_PROXY_COUNT`), 12-factor and safe for the OSS/self-host story.

## Architecture

### Component 1 — Real client IP via a trusted proxy

chi ships `middleware.ClientIPFromXFFTrustedProxies(n)` (present in the pinned `chi/v5 v5.3.1`). It parses `X-Forwarded-For`, selecting the entry at `len(xff) - n` — the IP the outermost trusted proxy observed, the only one an attacker cannot forge — and **stores it in a request-context value** read via `middleware.GetClientIP(ctx)`. It does **not** rewrite `r.RemoteAddr`, and it fails closed (sets nothing) when the header is missing or shorter than `n`. It panics at startup if `n < 1`.

- **Config:** new field `config.Config.TrustedProxyCount int`, parsed from env `TRUSTED_PROXY_COUNT`, **default `0`** (treat as direct — no proxy trusted), validated `>= 0`. Prod `.env` sets `1` (one Caddy hop). Not a required var.
- **Wiring (`app.New`):** register the chi middleware at router level only when configured, so dev/tests without a proxy are unaffected and the `n < 1` panic is never hit:
  ```go
  if cfg.TrustedProxyCount > 0 {
      router.Use(middleware.ClientIPFromXFFTrustedProxies(cfg.TrustedProxyCount))
  }
  ```
  Registered as a `chi` middleware (not a huma one) so the context value is set before humachi dispatches; the limiter later reads it via `ctx.Context()`.
- **`clientIP` helper (in `ratelimit.go`)** reads the chi-resolved IP first, falling back to the TCP peer when no trusted proxy is configured (dev/tests) or the header is absent:
  ```go
  func clientIP(ctx huma.Context) string {
      if ip := middleware.GetClientIP(ctx.Context()); ip != "" { return ip }
      if host, _, err := net.SplitHostPort(ctx.RemoteAddr()); err == nil { return host }
      return ctx.RemoteAddr()
  }
  ```
- The stale `RealIP`-deprecation comment in `app.go` is replaced with a note describing the now-live proxy topology and the `TRUSTED_PROXY_COUNT` knob.

### Component 2 — Two-tier limiter

Middleware chain in `app.New` becomes:

```
rl.LimitByIP → authn.Authenticate → rl.LimitByPrincipal → authn.Authorize
```

**Tier 1 — `LimitByIP`** runs **first**, before any DB work, keyed purely on the real client IP. Class is selected by path (consistent with the existing `strings.HasPrefix(path, "/api/v1/auth/")` style — no new metadata plumbing):

| Class   | Matches (operation path)     | Bucket key | Default budget |
|---------|------------------------------|------------|----------------|
| health  | `== "/healthz"`              | `h:<ip>`   | 60 / min       |
| auth    | `HasPrefix "/api/v1/auth/"`  | `a:<ip>`   | 10 / min       |
| general | everything else (DoS backstop)| `g:<ip>`  | 3000 / min     |

Tier 1 is the entire fix for problem 2's bypass: **there is no token keying pre-auth**. Rotating garbage tokens from one IP all share that IP's `g:` bucket and are 429'd *before* reaching `Authenticate`'s `GetDeviceSession` query. It also gives `/healthz` its own isolated class (problem 1's sub-issue), so an uptime monitor and an auth flood cannot starve each other.

**Tier 2 — `LimitByPrincipal`** runs **after `Authenticate`**, skips public ops (they carry no principal), and keys on the *verified* principal `p:<tenantID>:<userID>` at the normal budget (300/min). This is the "move token-keying after Authenticate" fix: it restores fair per-worker quota even when many workers share one carrier-NAT IP (under the coarse `g:` ceiling), and it is un-bypassable because the principal is cryptographically verified and garbage never reaches it. If (defensively) no principal is present on a non-public op, it passes through — `Authenticate` would already have rejected such a request.

Both tiers reuse the existing `writeProblem(ctx, 429, "rate-limited", "too many requests")` and share one bucket map (keys namespaced by the class prefixes above, so no collisions).

**Problem 3's "hash the map key" is superseded:** keys are only IP strings and principal UUIDs (both already public — UUIDs appear in every JWT), never secret token material, so there is nothing to hash. No plaintext token is ever stored.

### Component 3 — Bounded bucket map (lazy TTL eviction)

Each entry wraps its `*rate.Limiter` with a `lastSeen` timestamp:

```go
type bucket struct { lim *rate.Limiter; lastSeen time.Time }
```

`limiterFor` updates `lastSeen` on every access and runs a **lazy sweep under the existing mutex** — no background goroutine, no lifecycle/`Stop` to manage, no goroutine leak in tests:

- Sweep triggers when the map exceeds `maxBuckets` (a high safety cap, e.g. 100_000) **or** a sweep interval (e.g. a few minutes) has elapsed; it deletes buckets idle longer than `idleTTL`. These bounds are implementation constants, not contract.
- `idleTTL` (15 min) ≫ the 1-min token-refill window, so evicting an idle bucket never resets an actively-throttled client — after 15 min idle it would have refilled to full anyway, making a fresh bucket equivalent.
- A `now func() time.Time` field (default `time.Now`) makes eviction deterministically testable without sleeps.

### Component 4 — API surface

`NewRateLimiter` takes a config struct (four self-documenting tunable knobs) instead of two positional ints:

```go
type RateLimitConfig struct {
    AuthPerMin      int // /api/v1/auth/* per IP (brute-force target)
    HealthzPerMin   int // /healthz per IP (isolated)
    IPCeilingPerMin int // pre-auth ceiling on all other routes, per IP (DoS backstop)
    PrincipalPerMin int // authenticated, per verified principal (fair per-user quota)
}
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter
```

`app.New` call site:
```go
rl := auth.NewRateLimiter(auth.RateLimitConfig{
    AuthPerMin: 10, HealthzPerMin: 60, IPCeilingPerMin: 3000, PrincipalPerMin: 300,
})
```
Budgets are tunable, not contract (per the auth spec). Single limiter struct, two exported middleware methods (`LimitByIP`, `LimitByPrincipal`).

## Data flow

- **Public `/api/v1/auth/login` behind Caddy:** chi middleware sets client IP from XFF → `LimitByIP` keys `a:<realIP>` at 10/min → `Authenticate` skips (public) → `LimitByPrincipal` skips (public) → handler.
- **Authenticated worker `PUT /worker/executions/{id}` behind CGNAT:** `LimitByIP` keys `g:<sharedNATip>` at 3000/min (coarse ceiling) → `Authenticate` verifies device token, sets principal → `LimitByPrincipal` keys `p:<tenant>:<user>` at 300/min (the binding, fair limit) → `Authorize` → handler.
- **Attacker rotating garbage tokens from one IP:** `LimitByIP` keys `g:<ip>`; after 3000/min they get 429 **before** `Authenticate`'s DB query. No per-token buckets are ever created.
- **Uptime monitor:** `LimitByIP` keys `h:<monitorIP>` at 60/min, isolated from auth and general classes.

## Testing

- **Unit (`package auth`, white-box):**
  - class selection by path (`/healthz` → health, `/api/v1/auth/x` → auth, other → general);
  - **bypass regression:** many requests with *different* bearer tokens but the same client IP share one `g:` bucket and 429 after the ceiling;
  - per-principal keying: two injected principals (via the in-package `withPrincipal`) get independent buckets; one principal is limited at its budget;
  - eviction with the injected clock: stale buckets (`lastSeen` older than `idleTTL`) are swept, fresh ones and a bucket accessed within `idleTTL` are retained (its throttle state persists).
- **Integration (`package app_test`, `httptest` against the chi router):** with `TrustedProxyCount = 1`, drive the **auth class** (`/api/v1/auth/*`, the cheapest budget at 10/min) so separation is observable in ~11 requests: exhausting the `a:` bucket for `X-Forwarded-For: A` (11th → 429) does **not** 429 a request with `X-Forwarded-For: B` at the same TCP `RemoteAddr` (simulating Caddy). This is the concrete proof the collapse is fixed. A companion case with count `0` asserts XFF is ignored and the TCP peer is used.
- Existing `TestRateLimiterBlocksBurstExcess` is updated to the new constructor/method names.
- Test DBs use testcontainers (Podman backend env per the auth plan's Global Constraints).

## Docs updated in the same PR (per AGENTS.md)

- `.env.example` — add `TRUSTED_PROXY_COUNT=0` with a comment (dev direct; prod behind Caddy sets `1`).
- `deploy/docker-compose.yml` — plumb `TRUSTED_PROXY_COUNT: ${TRUSTED_PROXY_COUNT}` into the `api` service env.
- `docs/10-deployment.md` — Security-posture/topology note: app-level rate limiting on the real client IP (via one trusted proxy hop) plus a per-principal tier; `TRUSTED_PROXY_COUNT` config.
- `docs/07-api-spec.md` — line ~164 currently reads "Rate limiting: per-token …"; update to the real-client-IP shield + per-principal model with an isolated `/healthz` class.

## Out of scope (noted, not built)

- A pre-DB token *shape* check in `Authenticate` (reject dotless garbage before `GetDeviceSession`). Related, but it touches auth rather than the limiter, and Tier 1's `g:` IP ceiling already bounds that DB cost. Left as a possible future add.
- Distributed/shared rate-limit state (Redis). Single-binary, single-VPS topology (docs/10) makes the in-process map correct; revisit only if the API is ever horizontally scaled.
- Tuning the default budgets against real traffic — the values are code-tunable and will be revisited with production data.
