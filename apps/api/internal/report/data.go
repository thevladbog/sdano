// Package report aggregates the sqlc report queries into a print-ready
// ReportData tree and renders it to HTML (docs/09-pdf-report.md). It knows
// nothing about S3 or headless Chrome — photo bytes arrive through the
// injected PhotoLoader, and PDF rendering is task 3's job.
package report

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"

	"sdano.app/api/internal/db"
)

// ReportData is the fully-resolved tree the HTML template renders.
type ReportData struct {
	ShortID      string
	TenantName   string
	ContractName string
	ClientName   string
	PeriodFrom   time.Time
	PeriodTo     time.Time
	GeneratedAt  time.Time
	Summary      SummaryData
	Objects      []ObjectSection
	Missed       []MissedRow
}

// SummaryData is the inspector's page: headline numbers plus a per-object
// breakdown.
type SummaryData struct {
	ObjectCount   int
	Planned       int
	Done          int
	Missed        int
	CompletionPct int
	PerObject     []SummaryRow
}

// SummaryRow is one line of the summary table.
type SummaryRow struct {
	Name    string
	Address string
	Planned int
	Done    int
	Missed  int
}

// ObjectSection is one object's per-object section: every completed job in
// the period, in chronological order.
type ObjectSection struct {
	Name    string
	Address string
	Jobs    []JobRow
}

// JobRow is one completed execution: date, device completion time, worker,
// checklist result, and its photo grid.
type JobRow struct {
	Date         string
	FinishedAt   string
	WorkerName   string
	CheckedItems int
	TotalItems   int
	Photos       []PhotoCell
}

// PhotoCell is one cell of a job's photo grid. Missing=true means the photo
// was never confirmed uploaded — it renders as an explicit placeholder
// rather than being silently skipped (AGENTS.md: evidence is sacred).
type PhotoCell struct {
	DataURI string
	Caption string
	Missing bool
}

// MissedRow is one missed order, listed explicitly in the summary so the
// report stays honest as a dispute artifact.
type MissedRow struct {
	ObjectName string
	Date       string
}

// ClaimedReport is the report-queue row task 4's worker claims and hands to
// BuildReportData.
type ClaimedReport struct {
	ID         uuid.UUID
	TenantID   uuid.UUID
	ContractID uuid.NullUUID
	PeriodFrom time.Time
	PeriodTo   time.Time
}

// PhotoLoader resolves a photo's S3 key to a data URI suitable for an <img
// src>. Injected so this package needs no S3 client — Task 3 supplies the
// real implementation (download original, downscale, base64-encode); tests
// supply a fake.
type PhotoLoader func(ctx context.Context, s3Key string) (dataURI string, err error)

// ShortIDFor derives the human-referenceable report id used in disputes
// ("see report SD-3F8A, page 12"): "SD-" plus the uppercase hex of the
// UUID's first 4 bytes.
func ShortIDFor(id uuid.UUID) string {
	return fmt.Sprintf("SD-%X", id[:4])
}

const dateLayout = "02.01.2006"
const timeLayout = "15:04"

// BuildReportData runs the five task-1 report queries for the claimed
// tenant/contract/period, groups executions by object, and resolves photos
// through photoLoad. Unconfirmed photos (uploaded_at null) become
// PhotoCell{Missing: true} without ever calling photoLoad — there is nothing
// to load, and calling it would be a wasted S3 round trip.
func BuildReportData(ctx context.Context, q *db.Queries, r ClaimedReport, photoLoad PhotoLoader) (ReportData, error) {
	periodFrom := pgtype.Date{Time: r.PeriodFrom, Valid: true}
	periodTo := pgtype.Date{Time: r.PeriodTo, Valid: true}

	tenant, err := q.GetTenantForReport(ctx, r.TenantID)
	if err != nil {
		return ReportData{}, fmt.Errorf("loading tenant: %w", err)
	}
	// Report times (job completion, photo captions, generation date) print in
	// the tenant's wall clock — the zone the worker and the inspector live in
	// (docs/09: "completion time (device time)"). tenant.timezone is validated
	// at write time (SetTenantTimezone checks pg_timezone_names) and defaults
	// to 'UTC' (migration 0003), so a load failure is a can't-happen; fall
	// back to UTC with a warning (the workorder.TenantToday precedent) rather
	// than fail the render. Determinism holds either way: same stored instant
	// + same tenant timezone → same output on any render host.
	loc, locErr := time.LoadLocation(tenant.Timezone)
	if locErr != nil {
		slog.Warn("invalid tenant timezone, falling back to UTC", "tenant", r.TenantID, "timezone", tenant.Timezone)
		loc = time.UTC
	}

	var contractName, clientName string
	if r.ContractID.Valid {
		c, err := q.GetContractName(ctx, db.GetContractNameParams{ID: r.ContractID.UUID, TenantID: r.TenantID})
		if err != nil {
			return ReportData{}, fmt.Errorf("loading contract name: %w", err)
		}
		contractName = c.Name
		if c.ClientName != nil {
			clientName = *c.ClientName
		}
	}

	summaryRows, err := q.ReportSummaryRows(ctx, db.ReportSummaryRowsParams{
		TenantID: r.TenantID, PeriodFrom: periodFrom, PeriodTo: periodTo, ContractID: r.ContractID,
	})
	if err != nil {
		return ReportData{}, fmt.Errorf("loading summary rows: %w", err)
	}

	executions, err := q.ReportObjectExecutions(ctx, db.ReportObjectExecutionsParams{
		TenantID: r.TenantID, PeriodFrom: periodFrom, PeriodTo: periodTo, ContractID: r.ContractID,
	})
	if err != nil {
		return ReportData{}, fmt.Errorf("loading object executions: %w", err)
	}

	photosByExecution, err := loadPhotosByExecution(ctx, q, r.TenantID, executions)
	if err != nil {
		return ReportData{}, err
	}

	missedRows, err := q.ReportMissedOrders(ctx, db.ReportMissedOrdersParams{
		TenantID: r.TenantID, PeriodFrom: periodFrom, PeriodTo: periodTo, ContractID: r.ContractID,
	})
	if err != nil {
		return ReportData{}, fmt.Errorf("loading missed orders: %w", err)
	}

	summary, objectOrder, objectMeta := buildSummary(summaryRows)

	for _, e := range executions {
		meta, ok := objectMeta[e.ObjectID]
		if !ok {
			// Defensive: ReportObjectExecutions and ReportSummaryRows share
			// the same tenant/period/contract filter, so every execution's
			// object must already have a summary row.
			continue
		}
		job := JobRow{
			// Date comes from a pgtype.Date (UTC-midnight, no instant
			// semantics) — formatted as stored, never zone-converted: shifting
			// a calendar date through a timezone could move it to the
			// neighboring day.
			Date: e.DueDate.Time.Format(dateLayout),
			// .In(loc), never bare .Format: pgx decodes timestamptz into the
			// process's LOCAL zone, so formatting without an explicit zone
			// would print a different clock reading depending on the render
			// host. Reports are immutable evidence — pin the tenant zone.
			FinishedAt:   e.DeviceFinishedAt.Time.In(loc).Format(timeLayout),
			WorkerName:   e.WorkerName,
			CheckedItems: int(e.CheckedItems),
			TotalItems:   int(e.TotalItems),
		}
		for _, p := range photosByExecution[e.ExecutionID] {
			cell, err := buildPhotoCell(ctx, p, loc, photoLoad)
			if err != nil {
				return ReportData{}, err
			}
			job.Photos = append(job.Photos, cell)
		}
		meta.jobs = append(meta.jobs, job)
	}

	objects := make([]ObjectSection, 0, len(objectOrder))
	for _, id := range objectOrder {
		meta := objectMeta[id]
		objects = append(objects, ObjectSection{Name: meta.name, Address: meta.address, Jobs: meta.jobs})
	}

	missed := make([]MissedRow, 0, len(missedRows))
	for _, m := range missedRows {
		missed = append(missed, MissedRow{ObjectName: m.ObjectName, Date: m.DueDate.Time.Format(dateLayout)})
	}

	return ReportData{
		ShortID:      ShortIDFor(r.ID),
		TenantName:   tenant.Name,
		ContractName: contractName,
		ClientName:   clientName,
		PeriodFrom:   r.PeriodFrom,
		PeriodTo:     r.PeriodTo,
		GeneratedAt:  time.Now().In(loc),
		Summary:      summary,
		Objects:      objects,
		Missed:       missed,
	}, nil
}

// objectAccum accumulates one object's name/address (from the summary rows)
// and its job rows (from the executions) while preserving the query's
// address-sorted order.
type objectAccum struct {
	name, address string
	jobs          []JobRow
}

// buildSummary turns the raw summary rows into SummaryData plus a lookup
// keyed by object id, preserving ReportSummaryRows' ORDER BY address, name.
func buildSummary(rows []db.ReportSummaryRowsRow) (SummaryData, []uuid.UUID, map[uuid.UUID]*objectAccum) {
	summary := SummaryData{
		ObjectCount: len(rows),
		PerObject:   make([]SummaryRow, 0, len(rows)),
	}
	order := make([]uuid.UUID, 0, len(rows))
	meta := make(map[uuid.UUID]*objectAccum, len(rows))

	for _, s := range rows {
		addr := ""
		if s.Address != nil {
			addr = *s.Address
		}
		summary.Planned += int(s.Planned)
		summary.Done += int(s.Done)
		summary.Missed += int(s.Missed)
		summary.PerObject = append(summary.PerObject, SummaryRow{
			Name: s.ObjectName, Address: addr,
			Planned: int(s.Planned), Done: int(s.Done), Missed: int(s.Missed),
		})
		order = append(order, s.ObjectID)
		meta[s.ObjectID] = &objectAccum{name: s.ObjectName, address: addr}
	}
	if summary.Planned > 0 {
		summary.CompletionPct = summary.Done * 100 / summary.Planned
	}
	return summary, order, meta
}

// loadPhotosByExecution fetches every photo attached to the given
// executions in one query and groups them by execution id.
func loadPhotosByExecution(ctx context.Context, q *db.Queries, tenantID uuid.UUID, executions []db.ReportObjectExecutionsRow) (map[uuid.UUID][]db.ReportExecutionPhotosRow, error) {
	if len(executions) == 0 {
		return nil, nil
	}
	executionIDs := make([]uuid.UUID, len(executions))
	for i, e := range executions {
		executionIDs[i] = e.ExecutionID
	}
	photos, err := q.ReportExecutionPhotos(ctx, db.ReportExecutionPhotosParams{
		TenantID: tenantID, ExecutionIds: executionIDs,
	})
	if err != nil {
		return nil, fmt.Errorf("loading execution photos: %w", err)
	}
	byExecution := make(map[uuid.UUID][]db.ReportExecutionPhotosRow, len(executionIDs))
	for _, p := range photos {
		// photo.execution_id is schema-nullable (a photo may instead hang
		// off issue_id/resolution_id) but ReportExecutionPhotos filters on
		// execution_id = ANY(...), so every row here has one set — check
		// defensively rather than assume (task-1-report.md).
		if !p.ExecutionID.Valid {
			continue
		}
		byExecution[p.ExecutionID.UUID] = append(byExecution[p.ExecutionID.UUID], p)
	}
	return byExecution, nil
}

// buildPhotoCell converts one photo row into a PhotoCell. Unconfirmed
// uploads never reach photoLoad.
func buildPhotoCell(ctx context.Context, p db.ReportExecutionPhotosRow, loc *time.Location, photoLoad PhotoLoader) (PhotoCell, error) {
	if !p.UploadedAt.Valid {
		return PhotoCell{Missing: true}, nil
	}
	dataURI, err := photoLoad(ctx, p.S3Key)
	if err != nil {
		return PhotoCell{}, fmt.Errorf("loading photo %s: %w", p.ID, err)
	}
	return PhotoCell{DataURI: dataURI, Caption: photoCaption(p, loc)}, nil
}

// photoCaption renders "HH:MM · lat, lon" (capture time in the tenant's
// zone), skipping coordinates when absent.
func photoCaption(p db.ReportExecutionPhotosRow, loc *time.Location) string {
	caption := ""
	if p.TakenAt.Valid {
		caption = p.TakenAt.Time.In(loc).Format(timeLayout)
	}
	if p.Lat != nil && p.Lon != nil {
		coords := fmt.Sprintf("%.5f, %.5f", *p.Lat, *p.Lon)
		if caption == "" {
			return coords
		}
		return caption + " · " + coords
	}
	return caption
}
