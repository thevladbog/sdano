package auth

import (
	"net"
	"net/http"
	"sync"

	"github.com/danielgtaylor/huma/v2"
	"golang.org/x/time/rate"
)

// RateLimiter throttles requests: public/auth ops by client IP (strict), other
// ops by bearer token (generous). Buckets are kept in a mutex-guarded map;
// cardinality is low (a handful of tokens/IPs at this scale) so eviction is a
// later concern. Limits are tunable, not contract (see spec).
type RateLimiter struct {
	authLimit rate.Limit
	authBurst int
	normLimit rate.Limit
	normBurst int

	mu      sync.Mutex
	buckets map[string]*rate.Limiter
}

// NewRateLimiter builds a limiter from per-minute request budgets.
func NewRateLimiter(authPerMin, normalPerMin int) *RateLimiter {
	return &RateLimiter{
		authLimit: rate.Limit(float64(authPerMin) / 60.0),
		authBurst: authPerMin,
		normLimit: rate.Limit(float64(normalPerMin) / 60.0),
		normBurst: normalPerMin,
		buckets:   make(map[string]*rate.Limiter),
	}
}

func (rl *RateLimiter) Middleware(ctx huma.Context, next func(huma.Context)) {
	var key string
	var limit rate.Limit
	var burst int
	if isPublic(ctx.Operation()) {
		key, limit, burst = "ip:"+clientIP(ctx), rl.authLimit, rl.authBurst
	} else if tok := bearer(ctx); tok != "" {
		key, limit, burst = "tok:"+tok, rl.normLimit, rl.normBurst
	} else {
		key, limit, burst = "ip:"+clientIP(ctx), rl.normLimit, rl.normBurst
	}
	if !rl.limiterFor(key, limit, burst).Allow() {
		writeProblem(ctx, http.StatusTooManyRequests, "rate-limited", "too many requests")
		return
	}
	next(ctx)
}

func (rl *RateLimiter) limiterFor(key string, limit rate.Limit, burst int) *rate.Limiter {
	rl.mu.Lock()
	defer rl.mu.Unlock()
	l, ok := rl.buckets[key]
	if !ok {
		l = rate.NewLimiter(limit, burst)
		rl.buckets[key] = l
	}
	return l
}

// clientIP extracts the connecting peer's address, stripping the port if
// present. There is no trusted-proxy header parsing here (see the RealIP
// note in app.go) — this is the direct TCP peer, which is what we want for
// a middleware guarding against a single misbehaving client.
func clientIP(ctx huma.Context) string {
	if host, _, err := net.SplitHostPort(ctx.RemoteAddr()); err == nil {
		return host
	}
	return ctx.RemoteAddr()
}
