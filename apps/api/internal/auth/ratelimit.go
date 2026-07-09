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
	// maxBuckets caps the tracked-bucket count; once reached, new keys fail open
	// instead of growing the map. Overridable in tests.
	maxBuckets int
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
	// sweepInterval bounds how often we scan the map for idle buckets, so the
	// O(n) scan cost is capped regardless of request rate.
	sweepInterval = 5 * time.Minute
	// defaultMaxBuckets caps the tracked-bucket count. Once reached, new keys
	// fail open (see allow) rather than grow the map without bound — a DoS
	// backstop far above realistic cardinality at this scale.
	defaultMaxBuckets = 100_000

	healthzPath = "/healthz"
	authPrefix  = "/api/v1/auth/"
)

// NewRateLimiter builds a limiter from per-minute request budgets.
func NewRateLimiter(cfg RateLimitConfig) *RateLimiter {
	return &RateLimiter{
		auth:       newLimit(cfg.AuthPerMin),
		healthz:    newLimit(cfg.HealthzPerMin),
		ipCeiling:  newLimit(cfg.IPCeilingPerMin),
		principal:  newLimit(cfg.PrincipalPerMin),
		buckets:    make(map[string]*bucket),
		maxBuckets: defaultMaxBuckets,
		now:        time.Now,
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
		if len(rl.buckets) >= rl.maxBuckets {
			// Hard cap: the map is full of buckets sweep could not reclaim (all
			// still active). Fail open for this new key rather than grow without
			// bound — every bucket already tracked stays enforced. Only reachable
			// under an extreme distinct-key flood, where admitting untracked new
			// keys is safer than unbounded memory.
			return true
		}
		b = &bucket{lim: rate.NewLimiter(l.rate, l.burst)}
		rl.buckets[key] = b
	}
	b.lastSeen = now
	return b.lim.Allow()
}

// sweep evicts idle buckets. It runs on bucket creation but does real work at
// most once per sweepInterval, so the O(n) scan cost stays bounded regardless of
// request rate; growth between sweeps is capped by allow's maxBuckets fail-open.
// The caller holds rl.mu.
func (rl *RateLimiter) sweep(now time.Time) {
	if now.Sub(rl.lastSweep) < sweepInterval {
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
