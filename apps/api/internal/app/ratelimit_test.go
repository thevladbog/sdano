package app_test

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestRateLimitIgnoresSpoofedXFFPrefix proves that with one trusted proxy, only
// the rightmost X-Forwarded-For entry (the value the proxy appends from the peer
// it observed) keys the limiter — an attacker-controlled leftmost value cannot
// move the key. Each request sends a two-entry chain "<spoofed>, <realPeer>".
func TestRateLimitIgnoresSpoofedXFFPrefix(t *testing.T) {
	pool := testdb.New(t)
	router, _ := app.New(config.Config{TrustedProxyCount: 1}, app.Deps{Pool: pool})

	post := func(spoofed, realPeer string) int {
		body := []byte(`{"email":"nobody@example.test","password":"whatever-12345"}`)
		req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Forwarded-For", spoofed+", "+realPeer)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code
	}

	// Exhaust the auth bucket for real peer 203.0.113.30, varying the spoofed
	// prefix each time — if the spoof mattered, each would get a fresh bucket.
	for i := 1; i <= 10; i++ {
		if code := post("10.0.0."+strconv.Itoa(i), "203.0.113.30"); code == http.StatusTooManyRequests {
			t.Fatalf("request %d was rate-limited too early", i)
		}
	}
	// A brand-new spoofed prefix but the SAME real peer is still limited: the key
	// came from the trusted rightmost entry, not the spoofable prefix.
	if code := post("10.0.0.250", "203.0.113.30"); code != http.StatusTooManyRequests {
		t.Fatalf("spoofed prefix moved the rate-limit key: got %d, want 429", code)
	}
	// A different real peer (rightmost) is a separate bucket.
	if code := post("10.0.0.1", "203.0.113.31"); code == http.StatusTooManyRequests {
		t.Fatalf("different trusted peer must be a separate bucket: got %d, want non-429", code)
	}
}
