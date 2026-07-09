package workorder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
)

// ErrWorkOrderNotAssigned is returned when a worker tries to execute an order
// that is not theirs (or does not exist for their tenant).
var ErrWorkOrderNotAssigned = errors.New("work order not assigned to this worker")

// ErrExecutionIDConflict is returned when the path's execution id already
// belongs to a different tenant/worker (e.g. a client-generated UUID
// collision, since work_execution.id is a global PK with no per-tenant
// namespace — see db/migrations "Client-generated UUIDs (offline
// idempotency)"). UpsertWorkExecution's ON CONFLICT ... WHERE guard silently
// no-ops the row write in that case rather than erroring; this check makes
// that failure loud and stops the request before it can prune or overwrite
// another tenant's execution items (which are not themselves tenant-scoped).
var ErrExecutionIDConflict = errors.New("execution id already in use by another tenant or worker")

type ExecutionItemInput struct {
	ID             uuid.UUID
	TemplateItemID uuid.UUID
	Checked        bool
	CheckedAt      *time.Time
}

type ExecutionInput struct {
	WorkOrderID      uuid.UUID
	StartedAt        *time.Time
	DeviceFinishedAt *time.Time
	Note             *string
	Items            []ExecutionItemInput
}

func ts(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func tptr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// UpsertExecution validates the order belongs to the worker, then applies the
// full-state snapshot (execution + items + order status) in one transaction.
// Idempotent by construction: replaying any snapshot converges. finished_at is
// stamped once (server clock) when device_finished_at first appears.
func UpsertExecution(ctx context.Context, pool *pgxpool.Pool, tenantID, workerID, executionID uuid.UUID, in ExecutionInput) error {
	q := db.New(pool)
	wo, err := q.GetWorkOrderForWorker(ctx, db.GetWorkOrderForWorkerParams{ID: in.WorkOrderID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkOrderNotAssigned
	}
	if err != nil {
		return fmt.Errorf("loading work order: %w", err)
	}
	if !wo.AssigneeID.Valid || wo.AssigneeID.UUID != workerID {
		return ErrWorkOrderNotAssigned
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := q.WithTx(tx)

	if err := qtx.UpsertWorkExecution(ctx, db.UpsertWorkExecutionParams{
		ID: executionID, TenantID: tenantID, WorkOrderID: in.WorkOrderID, WorkerID: workerID,
		StartedAt: ts(in.StartedAt), DeviceFinishedAt: ts(in.DeviceFinishedAt), Note: in.Note,
	}); err != nil {
		return fmt.Errorf("upsert execution: %w", err)
	}

	// Confirm the write actually landed under this tenant/worker. If
	// executionID collided with another tenant's row, the ON CONFLICT WHERE
	// guard in UpsertWorkExecution silently skipped the update — proceeding
	// past that point would let DeleteExecutionItemsNotIn and
	// UpsertWorkExecutionItem (neither of which are tenant-scoped) mutate
	// that other tenant's evidence.
	owner, err := qtx.GetExecutionForWorker(ctx, db.GetExecutionForWorkerParams{ID: executionID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrExecutionIDConflict
	}
	if err != nil {
		return fmt.Errorf("verifying execution ownership: %w", err)
	}
	if owner.WorkerID != workerID {
		return ErrExecutionIDConflict
	}

	keep := make([]uuid.UUID, 0, len(in.Items))
	for _, it := range in.Items {
		keep = append(keep, it.ID)
	}
	if err := qtx.DeleteExecutionItemsNotIn(ctx, db.DeleteExecutionItemsNotInParams{ExecutionID: executionID, KeepIds: keep}); err != nil {
		return fmt.Errorf("pruning items: %w", err)
	}
	for _, it := range in.Items {
		if err := qtx.UpsertWorkExecutionItem(ctx, db.UpsertWorkExecutionItemParams{
			ID: it.ID, ExecutionID: executionID, TemplateItemID: it.TemplateItemID,
			Checked: it.Checked, CheckedAt: ts(it.CheckedAt),
		}); err != nil {
			return fmt.Errorf("upsert item %s: %w", it.ID, err)
		}
	}

	status := db.WorkOrderStatusInProgress
	if in.DeviceFinishedAt != nil {
		status = db.WorkOrderStatusDone
	}
	if err := qtx.SetWorkOrderStatus(ctx, db.SetWorkOrderStatusParams{ID: in.WorkOrderID, TenantID: tenantID, Status: status}); err != nil {
		return fmt.Errorf("set order status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ExecutionView is the server's view of an execution, returned by the upsert.
type ExecutionView struct {
	ID               uuid.UUID            `json:"id"`
	WorkOrderID      uuid.UUID            `json:"work_order_id"`
	StartedAt        *time.Time           `json:"started_at"`
	FinishedAt       *time.Time           `json:"finished_at"`
	DeviceFinishedAt *time.Time           `json:"device_finished_at"`
	Note             *string              `json:"note"`
	Items            []executionItemView  `json:"items"`
	Photos           []executionPhotoView `json:"photos"`
}

type executionItemView struct {
	ID             uuid.UUID  `json:"id"`
	TemplateItemID uuid.UUID  `json:"template_item_id"`
	Checked        bool       `json:"checked"`
	CheckedAt      *time.Time `json:"checked_at"`
}

type executionPhotoView struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	TakenAt    *time.Time `json:"taken_at"`
	Lat        *float64   `json:"lat"`
	Lon        *float64   `json:"lon"`
	UploadedAt *time.Time `json:"uploaded_at"`
}

func loadExecutionView(ctx context.Context, q *db.Queries, tenantID, executionID uuid.UUID) (ExecutionView, error) {
	e, err := q.GetExecution(ctx, db.GetExecutionParams{ID: executionID, TenantID: tenantID})
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading execution: %w", err)
	}
	items, err := q.ListExecutionItems(ctx, executionID)
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading items: %w", err)
	}
	photos, err := q.ListExecutionPhotos(ctx, uuid.NullUUID{UUID: executionID, Valid: true})
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading photos: %w", err)
	}
	v := ExecutionView{
		ID: e.ID, WorkOrderID: e.WorkOrderID,
		StartedAt: tptr(e.StartedAt), FinishedAt: tptr(e.FinishedAt), DeviceFinishedAt: tptr(e.DeviceFinishedAt),
		Note:   e.Note,
		Items:  make([]executionItemView, 0, len(items)),
		Photos: make([]executionPhotoView, 0, len(photos)),
	}
	for _, it := range items {
		v.Items = append(v.Items, executionItemView{ID: it.ID, TemplateItemID: it.TemplateItemID, Checked: it.Checked, CheckedAt: tptr(it.CheckedAt)})
	}
	for _, ph := range photos {
		v.Photos = append(v.Photos, executionPhotoView{ID: ph.ID, Kind: string(ph.Kind), TakenAt: tptr(ph.TakenAt), Lat: ph.Lat, Lon: ph.Lon, UploadedAt: tptr(ph.UploadedAt)})
	}
	return v, nil
}
