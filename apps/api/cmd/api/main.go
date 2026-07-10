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
	"sync"
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
	"sdano.app/api/internal/platform"
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

	// wg tracks the two background goroutines below so run() doesn't return
	// (and defer pool.Close() doesn't fire) while either is still mid-tick.
	// The wait is short: both Run loops exit promptly on ctx.Done (render
	// work aborts via its child ctx; the fail-mark/sweep write is bounded by
	// its own short detached timeout), so this never meaningfully delays
	// shutdown.
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		reportWorker.Run(ctx)
	}()

	// Hourly scheduler: tenant-timezone-aware missed-order marking + orphan
	// photo GC (task 6). Shares the same store and signal ctx as the report
	// worker — both stop on the same shutdown signal.
	scheduler := platform.NewScheduler(pool, store)
	wg.Add(1)
	go func() {
		defer wg.Done()
		scheduler.Run(ctx)
	}()

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
		// The server died on its own (no signal): cancel the background ctx
		// explicitly, or wg.Wait would block forever on the still-running
		// loops.
		stop()
		wg.Wait()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			return fmt.Errorf("http server: %w", err)
		}
		return nil
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		shutdownErr := srv.Shutdown(shutdownCtx)
		// ctx is already cancelled at this point, so the report worker and
		// scheduler loops are already exiting (or have exited); this just
		// waits for that to actually finish. Both loops exit promptly on
		// ctx.Done (in-flight renders abort via a child ctx; the detached
		// fail-mark write is bounded by its own short timeout), so the wait
		// here is short.
		wg.Wait()
		if shutdownErr != nil {
			return fmt.Errorf("graceful shutdown: %w", shutdownErr)
		}
		logger.Info("api stopped")
		return nil
	}
}
