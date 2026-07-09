// Package app assembles the HTTP API: router, huma, middleware, and all
// route registrations. cmd/api and tests both build the app through New.
package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"sdano.app/api/internal/config"
)

// HealthCheck is a named dependency probe run by GET /healthz.
type HealthCheck struct {
	Name string
	Ping func(ctx context.Context) error
}

// Deps carries everything app.New wires into handlers. Grows with the app.
type Deps struct {
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
	humaCfg.DocsPath = "/docs"
	api := humachi.New(router, humaCfg)

	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Service health",
		Tags:        []string{"meta"},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		for _, c := range deps.Checks {
			if err := c.Ping(ctx); err != nil {
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
