package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

func TestHealthzReportsDBState(t *testing.T) {
	pool := testdb.New(t)

	router, _ := app.New(config.Config{}, app.Deps{
		Pool:   pool,
		Checks: []app.HealthCheck{app.DBCheck(pool)},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy DB: got %d, want 200; body: %s", rec.Code, rec.Body)
	}

	pool.Close()
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed DB: got %d, want 503; body: %s", rec.Code, rec.Body)
	}
}
