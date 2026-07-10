package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	// Report rendering formats times via time.LoadLocation(tenant.timezone)
	// (internal/report/data.go). Embedding the IANA database guards minimal
	// container images (e.g. distroless, no /usr/share/zoneinfo) from
	// silently falling back to UTC instead of the tenant's zone.
	_ "time/tzdata"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/photo"
	"sdano.app/api/internal/report"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if len(os.Args) > 1 && os.Args[1] == "openapi" {
		if err := printOpenAPI(); err != nil {
			logger.Error("emitting openapi", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(logger); err != nil {
		logger.Error("api exited", "error", err)
		os.Exit(1)
	}
}

// printOpenAPI builds the app with zero deps (handlers register but never
// run) and dumps the OpenAPI 3.1 spec to stdout for orval.
func printOpenAPI() error {
	_, api := app.New(config.Config{}, app.Deps{})
	b, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling openapi spec: %w", err)
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	pool, err := app.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	s3c, err := app.NewS3(cfg)
	if err != nil {
		return fmt.Errorf("building s3 client: %w", err)
	}

	router, _ := app.New(cfg, app.Deps{
		Pool: pool,
		S3:   s3c,
		Checks: []app.HealthCheck{
			app.DBCheck(pool),
			app.S3Check(s3c, cfg.S3Bucket),
		},
	})

	// Separate ObjectStore instance for the report worker (app.New builds
	// its own internally from deps.S3 for the HTTP handlers) — cheap struct,
	// no reason to share it across the process's two consumers.
	store := photo.NewS3Store(s3c, cfg.S3Bucket)
	reportWorker := report.NewWorker(pool, store, report.NewChromeRenderer(cfg.ChromeCDPURL))
	go reportWorker.Run(ctx)

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		logger.Info("api stopped")
		return nil
	}
}
