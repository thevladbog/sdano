package report

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
	"sdano.app/api/internal/photo"
)

// pollInterval is how long Run sleeps after an empty-queue tick before
// checking the report queue again.
const pollInterval = 5 * time.Second

// maxRenderAttempts mirrors db/queries/report.sql's ClaimNextReport, which
// only claims rows with render_attempts < 3 (and increments on claim). A
// claimed row's RenderAttempts therefore always lands in [1,3] — 3 means
// this WAS the last allowed try.
const maxRenderAttempts = 3

// maxFailureReasonLen bounds MarkReportFailed's reason so a verbose
// downstream error (e.g. a raw chromedp/S3 error dump) never grows without
// limit in the failure_reason column.
const maxFailureReasonLen = 500

// Worker drains the report queue: report rows sitting in 'generating'
// status, produced by task 5's enqueue endpoint. It composes the pieces
// tasks 1-3 built (BuildReportData, RenderHTML, PDFRenderer, ObjectStore)
// and contains no rendering or SQL logic of its own beyond calling the
// generated queries.
type Worker struct {
	q        *db.Queries
	store    photo.ObjectStore
	renderer PDFRenderer
}

// NewWorker builds a Worker. store supplies both the original photo bytes
// (via the PhotoLoader injected into BuildReportData) and the destination
// for the rendered PDF.
func NewWorker(pool *pgxpool.Pool, store photo.ObjectStore, renderer PDFRenderer) *Worker {
	return &Worker{q: db.New(pool), store: store, renderer: renderer}
}

// Run loops tick+sleep(5s) until ctx is cancelled. main.go runs this against
// the process's signal context, so the worker stops on the same shutdown
// signal as the HTTP server.
func (w *Worker) Run(ctx context.Context) {
	for {
		if ctx.Err() != nil {
			return
		}
		w.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-time.After(pollInterval):
		}
	}
}

// tick claims and renders at most one queued report, returning whether a
// row was claimed (false on an empty queue). Exported-for-test seam: Run
// wraps tick with the poll sleep; tests call tick directly.
func (w *Worker) tick(ctx context.Context) bool {
	claimed, err := w.q.ClaimNextReport(ctx)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return false
		}
		slog.Error("claiming next report", "error", err)
		return false
	}

	if err := w.render(ctx, claimed); err != nil {
		slog.Error("rendering report", "report_id", claimed.ID, "attempts", claimed.RenderAttempts, "error", err)
		// Attempts already reflects this claim's increment (ClaimNextReport
		// only ever offers render_attempts < 3), so >= 3 means this was the
		// last retry — anything less leaves the row 'generating' for the
		// next poll to pick back up.
		if claimed.RenderAttempts >= maxRenderAttempts {
			reason := truncateReason(fmt.Sprintf("render failed after %d attempts: %v", claimed.RenderAttempts, err))
			if failErr := w.q.MarkReportFailed(ctx, db.MarkReportFailedParams{ID: claimed.ID, FailureReason: &reason}); failErr != nil {
				slog.Error("marking report failed", "report_id", claimed.ID, "error", failErr)
			}
		}
	}
	return true
}

// render runs the full build -> HTML -> PDF -> upload -> mark-ready chain
// for one claimed report. It never uploads on partial failure: store.Put is
// only reached after RenderPDF has returned successfully.
func (w *Worker) render(ctx context.Context, claimed db.ClaimNextReportRow) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}

	photoLoad := func(ctx context.Context, key string) (string, error) {
		raw, err := w.store.Get(ctx, key)
		if err != nil {
			return "", fmt.Errorf("loading photo %s: %w", key, err)
		}
		return DownscaleJPEG(raw)
	}

	data, err := BuildReportData(ctx, w.q, ClaimedReport{
		ID:         claimed.ID,
		TenantID:   claimed.TenantID,
		ContractID: claimed.ContractID,
		PeriodFrom: claimed.PeriodFrom.Time,
		PeriodTo:   claimed.PeriodTo.Time,
	}, photoLoad)
	if err != nil {
		return fmt.Errorf("building report data: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	html, err := RenderHTML(data)
	if err != nil {
		return fmt.Errorf("rendering html: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	pdf, err := w.renderer.RenderPDF(ctx, html)
	if err != nil {
		return fmt.Errorf("rendering pdf: %w", err)
	}

	if ctx.Err() != nil {
		return ctx.Err()
	}

	key := fmt.Sprintf("tenants/%s/reports/%s.pdf", claimed.TenantID, claimed.ID)
	if err := w.store.Put(ctx, key, "application/pdf", pdf); err != nil {
		return fmt.Errorf("uploading pdf: %w", err)
	}

	if err := w.q.MarkReportReady(ctx, db.MarkReportReadyParams{ID: claimed.ID, S3Key: &key}); err != nil {
		return fmt.Errorf("marking report ready: %w", err)
	}
	return nil
}

func truncateReason(s string) string {
	if len(s) <= maxFailureReasonLen {
		return s
	}
	return s[:maxFailureReasonLen]
}
