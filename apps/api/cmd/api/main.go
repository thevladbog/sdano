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

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
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

	router, _ := app.New(cfg, app.Deps{})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

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
