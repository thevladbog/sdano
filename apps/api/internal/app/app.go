// Package app assembles the HTTP API: router, huma, middleware, and all
// route registrations. cmd/api and tests both build the app through New.
package app

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/object"
)

// HealthCheck is a named dependency probe run by GET /healthz.
type HealthCheck struct {
	Name string
	Ping func(ctx context.Context) error
}

// Deps carries everything app.New wires into handlers. Grows with the app.
type Deps struct {
	Pool   *pgxpool.Pool
	Checks []HealthCheck
}

type healthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Overall service health"`
	}
}

func New(cfg config.Config, deps Deps) (*chi.Mux, huma.API) {
	router := chi.NewMux()
	router.Use(middleware.RequestID)
	// NOTE: middleware.RealIP is intentionally not used — it is deprecated
	// upstream as vulnerable to IP spoofing (GHSA-3fxj-6jh8-hvhx). Once a
	// reverse proxy with a known trusted-proxy CIDR list is introduced,
	// wire up middleware.ClientIPFromXFFTrustedProxies instead.

	humaCfg := huma.DefaultConfig("Sdano API", "0.1.0")
	humaCfg.Info.Description = "Photo-evidence and reporting platform for field service contractors."
	// Disable huma's built-in docs page (Stoplight Elements); we serve
	// Scalar's standalone viewer at /docs instead (see below), per docs/02.
	// The OpenAPI spec routes (/openapi.json etc.) stay enabled — Scalar
	// fetches the spec from there.
	humaCfg.DocsPath = ""
	api := humachi.New(router, humaCfg)

	// Scalar API reference at /docs, per docs/02-architecture.md
	// ("huma: typed handlers → OpenAPI 3.1 + built-in Scalar docs").
	router.Get("/docs", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<!doctype html>
<html>
<head>
  <title>Sdano API Docs</title>
  <meta charset="utf-8"/>
  <meta name="viewport" content="width=device-width, initial-scale=1"/>
</head>
<body>
  <script id="api-reference" data-url="/openapi.json"></script>
  <script src="https://cdn.jsdelivr.net/npm/@scalar/api-reference@1.62.5"></script>
</body>
</html>`))
	})

	queries := db.New(deps.Pool)
	authn := auth.NewAuthenticator(cfg.JWTSecret, queries)
	rl := auth.NewRateLimiter(10, 300) // 10/min on auth endpoints, 300/min authenticated
	api.UseMiddleware(rl.Middleware, authn.Authenticate, authn.Authorize)

	// Route registration only wires up schema + handler closures; it never
	// touches deps.Pool until a request actually runs. Registering
	// unconditionally means `go run ./cmd/api openapi` (which builds the app
	// with a nil pool) still emits listStaffObjects in the spec.
	object.Register(api, queries)
	auth.RegisterAuthRoutes(api, auth.NewService(deps.Pool, cfg.JWTSecret))

	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Service health",
		Tags:        []string{"meta"},
		Metadata:    auth.Public(),
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		for _, c := range deps.Checks {
			if err := c.Ping(ctx); err != nil {
				slog.Warn("health check failed", "dep", c.Name, "error", err)
				return nil, huma.Error503ServiceUnavailable(
					fmt.Sprintf("dependency %s unavailable", c.Name))
			}
		}
		out := &healthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	return router, api
}
