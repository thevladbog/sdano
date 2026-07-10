// Package platform holds the hourly background scheduler (this file) and,
// starting in task 7, the sdano-ops CLI's operator queries — both operate
// cross-tenant, unlike every other internal package which is scoped to a
// single authenticated tenant.
package platform

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
	"sdano.app/api/internal/photo"
)

// tickInterval is how often Run re-executes the scheduler's jobs.
const tickInterval = time.Hour

// orphanPhotoAge is how long a presigned-but-never-confirmed photo row is
// left alone before it becomes eligible for GC — long enough that a slow or
// retried mobile upload never races the sweep.
const orphanPhotoAge = 14 * 24 * time.Hour

// Scheduler runs the process's hourly background jobs: marking overdue
// scheduled work orders missed (tenant-timezone-aware, one tenant at a time
// via db/queries/platform.sql's MarkTenantOverdueOrdersMissed) and
// garbage-collecting orphaned photo rows (presigned for upload but never
// confirmed).
type Scheduler struct {
	q     *db.Queries
	store photo.ObjectStore
}

// NewScheduler builds a Scheduler. store is used only to verify whether an
// orphan photo's S3 object exists before its row is deleted — the evidence
// rule (AGENTS.md) requires that check, never a delete on uncertainty.
func NewScheduler(pool *pgxpool.Pool, store photo.ObjectStore) *Scheduler {
	return &Scheduler{q: db.New(pool), store: store}
}

// Run executes RunOnce immediately, then again every tickInterval, until ctx
// is cancelled. Mirrors report.Worker.Run's immediate-then-loop shape so
// main.go's shutdown signal stops both the same way.
func (s *Scheduler) Run(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	s.tick(ctx)

	ticker := time.NewTicker(tickInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.tick(ctx)
		}
	}
}

// tick runs RunOnce and logs any error it returns. Run's only caller of
// RunOnce, so the hourly loop and the process's shutdown never propagate a
// scheduler error further than a log line.
func (s *Scheduler) tick(ctx context.Context) {
	if err := s.RunOnce(ctx); err != nil {
		slog.Error("scheduler run encountered errors", "error", err)
	}
}

// RunOnce executes both jobs once: mark-overdue-orders-missed, then
// orphan-photo GC. The two are independent — one failing is wrapped and
// collected but never prevents the other from running. Exported for tests;
// Run wraps this with the hourly loop.
func (s *Scheduler) RunOnce(ctx context.Context) error {
	var errs []error

	if err := s.markOverdueMissed(ctx); err != nil {
		errs = append(errs, fmt.Errorf("marking overdue orders missed: %w", err))
	}

	if err := s.gcOrphanPhotos(ctx); err != nil {
		errs = append(errs, fmt.Errorf("gc orphan photos: %w", err))
	}

	return errors.Join(errs...)
}

// markOverdueMissed flips every tenant's overdue scheduled orders to missed,
// one tenant at a time. The tenant-local date is computed in Go (the same
// LoadLocation-with-UTC-fallback shape as workorder.TenantToday) so one
// tenant's invalid timezone degrades only that tenant to a UTC calendar —
// with a warning — instead of failing the whole cross-tenant sweep, and one
// tenant's DB error is collected without stopping the rest. Only logs when
// it actually changed something, so a quiet hour produces no log noise.
func (s *Scheduler) markOverdueMissed(ctx context.Context) error {
	tenants, err := s.q.ListTenantTimezones(ctx)
	if err != nil {
		return fmt.Errorf("listing tenant timezones: %w", err)
	}
	var errs []error
	var total int64
	for _, tenant := range tenants {
		loc, lerr := time.LoadLocation(tenant.Timezone)
		if lerr != nil {
			slog.Warn("invalid tenant timezone, falling back to UTC", "tenant", tenant.ID, "timezone", tenant.Timezone)
			loc = time.UTC
		}
		// Tenant-local "today" as a date-only value pinned to UTC — pgtype.Date
		// carries year/month/day only, so the Time's own zone must not shift
		// the calendar date (same shape as workorder.TenantToday).
		now := time.Now().In(loc)
		today := pgtype.Date{Time: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), Valid: true}
		n, err := s.q.MarkTenantOverdueOrdersMissed(ctx, db.MarkTenantOverdueOrdersMissedParams{
			TenantID: tenant.ID, TenantToday: today,
		})
		if err != nil {
			errs = append(errs, fmt.Errorf("tenant %s: %w", tenant.ID, err))
			continue
		}
		total += n
	}
	if total > 0 {
		slog.Info("marked overdue orders missed", "count", total)
	}
	return errors.Join(errs...)
}

// gcOrphanPhotos deletes photo rows that were presigned for upload but never
// confirmed, and are old enough (orphanPhotoAge) that the upload is
// considered abandoned — but ONLY after verifying the S3 object truly does
// not exist. A photo whose object turns out to exist (e.g. the client
// uploaded but the confirm call itself never landed) is left alone and
// logged loudly instead of silently losing evidence. A single photo's
// failure (Exists erroring, or the delete itself erroring) is logged and
// skipped rather than aborting the rest of the batch.
func (s *Scheduler) gcOrphanPhotos(ctx context.Context) error {
	cutoff := pgtype.Timestamptz{Time: time.Now().Add(-orphanPhotoAge), Valid: true}
	rows, err := s.q.ListOrphanPhotos(ctx, cutoff)
	if err != nil {
		return err
	}

	for _, row := range rows {
		exists, err := s.store.Exists(ctx, row.S3Key)
		if err != nil {
			// Uncertainty about whether the bytes exist must never be
			// treated as "safe to delete" (evidence rule) — log and leave
			// the row for the next hourly pass to retry.
			slog.Error("checking orphan photo existence", "photo_id", row.ID, "s3_key", row.S3Key, "error", err)
			continue
		}
		if exists {
			slog.Warn("orphan photo has S3 object, leaving row", "photo_id", row.ID, "s3_key", row.S3Key)
			continue
		}
		// DeletePhotoRow is guarded by `uploaded_at IS NULL` at the SQL
		// level — a defense-in-depth belt against ever deleting a
		// confirmed (evidence-bearing) row even if this query's own
		// filter were ever loosened.
		if err := s.q.DeletePhotoRow(ctx, row.ID); err != nil {
			slog.Error("deleting orphan photo row", "photo_id", row.ID, "error", err)
			continue
		}
	}
	return nil
}
