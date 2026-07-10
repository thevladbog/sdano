// Package workorder exposes the worker-facing planned-loop endpoints: the
// today bootstrap read and the idempotent execution upsert.
package workorder

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/photo"
)

// TenantToday computes "today" as a date in the tenant's IANA timezone
// (tenant.timezone, default UTC; invalid values fall back to UTC with a warning).
func TenantToday(ctx context.Context, q *db.Queries, tenantID uuid.UUID) (pgtype.Date, error) {
	tz, err := q.GetTenantTimezone(ctx, tenantID)
	if err != nil {
		return pgtype.Date{}, fmt.Errorf("loading tenant timezone: %w", err)
	}
	loc, lerr := time.LoadLocation(tz)
	if lerr != nil {
		slog.Warn("invalid tenant timezone, falling back to UTC", "tenant", tenantID, "timezone", tz)
		loc = time.UTC
	}
	now := time.Now().In(loc)
	return pgtype.Date{Time: time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC), Valid: true}, nil
}

// Register wires the worker planned-loop routes plus the staff-facing
// work-order and evidence-read routes onto api. It takes the pool (not just
// Queries) because the execution upsert added in Task 3 runs a transaction.
// Route registration never touches the pool until a request runs, so a nil
// pool (openapi mode) is fine. store is the object store used to presign
// GET URLs for the staff execution-detail photo list.
func Register(api huma.API, pool *pgxpool.Pool, store photo.ObjectStore) {
	q := db.New(pool)
	registerToday(api, q)
	registerExecutions(api, pool)
	registerStaffOrders(api, pool)
	registerStaffExecutionDetail(api, q, store)
	registerStaffDashboard(api, q)
}

type todayObject struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Address *string   `json:"address"`
	Lat     *float64  `json:"lat"`
	Lon     *float64  `json:"lon"`
	QRToken *string   `json:"qr_token"`
}

type checklistItem struct {
	ID            uuid.UUID `json:"id"`
	Position      int32     `json:"position"`
	Title         string    `json:"title"`
	RequiresPhoto bool      `json:"requires_photo"`
}

type checklist struct {
	VersionID uuid.UUID       `json:"version_id"`
	Items     []checklistItem `json:"items"`
}

type todayOrder struct {
	ID        uuid.UUID `json:"id"`
	ObjectID  uuid.UUID `json:"object_id"`
	DueDate   string    `json:"due_date"`
	Status    string    `json:"status"`
	Checklist checklist `json:"checklist"`
}

type todayOutput struct {
	Body struct {
		GeneratedAt time.Time     `json:"generated_at"`
		Objects     []todayObject `json:"objects"`
		WorkOrders  []todayOrder  `json:"work_orders"`
	}
}

func registerToday(api huma.API, q *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "workerToday",
		Method:      http.MethodGet,
		Path:        "/api/v1/worker/today",
		Summary:     "Worker's working set for today",
		Tags:        []string{"worker"},
	}, func(ctx context.Context, _ *struct{}) (*todayOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		now := time.Now().UTC()
		today, err := TenantToday(ctx, q, p.TenantID)
		if err != nil {
			return nil, fmt.Errorf("computing tenant-local today for tenant %s: %w", p.TenantID, err)
		}
		orders, err := q.ListWorkerTodayOrders(ctx, db.ListWorkerTodayOrdersParams{
			TenantID:   p.TenantID,
			AssigneeID: uuid.NullUUID{UUID: p.UserID, Valid: true},
			DueDate:    today,
		})
		if err != nil {
			return nil, fmt.Errorf("listing today orders for worker %s: %w", p.UserID, err)
		}

		versionSet := map[uuid.UUID]struct{}{}
		for _, o := range orders {
			versionSet[o.VersionID] = struct{}{}
		}
		versionIDs := make([]uuid.UUID, 0, len(versionSet))
		for v := range versionSet {
			versionIDs = append(versionIDs, v)
		}
		itemsByVersion := map[uuid.UUID][]checklistItem{}
		if len(versionIDs) > 0 {
			items, err := q.ListChecklistItemsByVersions(ctx, db.ListChecklistItemsByVersionsParams{
				VersionIds: versionIDs,
				TenantID:   p.TenantID,
			})
			if err != nil {
				return nil, fmt.Errorf("listing checklist items: %w", err)
			}
			for _, it := range items {
				itemsByVersion[it.VersionID] = append(itemsByVersion[it.VersionID], checklistItem{
					ID: it.ID, Position: it.Position, Title: it.Title, RequiresPhoto: it.RequiresPhoto,
				})
			}
		}

		out := &todayOutput{}
		out.Body.GeneratedAt = now
		out.Body.Objects = make([]todayObject, 0, len(orders))
		out.Body.WorkOrders = make([]todayOrder, 0, len(orders))
		seenObject := map[uuid.UUID]struct{}{}
		for _, o := range orders {
			if _, dup := seenObject[o.ObjectID]; !dup {
				seenObject[o.ObjectID] = struct{}{}
				out.Body.Objects = append(out.Body.Objects, todayObject{
					ID: o.ObjectID, Name: o.ObjectName, Address: o.Address,
					Lat: o.Lat, Lon: o.Lon, QRToken: o.QrToken,
				})
			}
			items := itemsByVersion[o.VersionID]
			if items == nil {
				items = []checklistItem{}
			}
			out.Body.WorkOrders = append(out.Body.WorkOrders, todayOrder{
				ID: o.ID, ObjectID: o.ObjectID,
				DueDate:   o.DueDate.Time.Format("2006-01-02"),
				Status:    string(o.Status),
				Checklist: checklist{VersionID: o.VersionID, Items: items},
			})
		}
		return out, nil
	})
}

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

type executionItemBody struct {
	ID             string     `json:"id" format:"uuid"`
	TemplateItemID string     `json:"template_item_id" format:"uuid"`
	Checked        bool       `json:"checked"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
}

type executionUpsertInput struct {
	ID   string `path:"id"`
	Body struct {
		WorkOrderID      string              `json:"work_order_id" format:"uuid"`
		StartedAt        *time.Time          `json:"started_at,omitempty"`
		DeviceFinishedAt *time.Time          `json:"device_finished_at,omitempty"`
		Note             *string             `json:"note,omitempty"`
		Items            []executionItemBody `json:"items"`
	}
}

type executionOutput struct {
	Body ExecutionView
}

func registerExecutions(api huma.API, pool *pgxpool.Pool) {
	huma.Register(api, huma.Operation{
		OperationID: "upsertWorkerExecution",
		Method:      http.MethodPut,
		Path:        "/api/v1/worker/executions/{id}",
		Summary:     "Idempotent full-state execution upsert",
		Tags:        []string{"worker"},
		Metadata:    auth.SuspendedWritable(),
	}, func(ctx context.Context, in *executionUpsertInput) (*executionOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		execID, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid execution id")
		}
		woID, err := uuid.Parse(in.Body.WorkOrderID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid work_order_id")
		}
		items := make([]ExecutionItemInput, 0, len(in.Body.Items))
		for _, it := range in.Body.Items {
			iid, err := uuid.Parse(it.ID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid item id")
			}
			tid, err := uuid.Parse(it.TemplateItemID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid template_item_id")
			}
			items = append(items, ExecutionItemInput{ID: iid, TemplateItemID: tid, Checked: it.Checked, CheckedAt: it.CheckedAt})
		}
		if err := UpsertExecution(ctx, pool, p.TenantID, p.UserID, execID, ExecutionInput{
			WorkOrderID: woID, StartedAt: in.Body.StartedAt, DeviceFinishedAt: in.Body.DeviceFinishedAt,
			Note: in.Body.Note, Items: items,
		}); errors.Is(err, ErrWorkOrderNotAssigned) {
			return nil, problem(http.StatusForbidden, "work-order-not-assigned", "this work order is not assigned to you")
		} else if errors.Is(err, ErrExecutionIDConflict) {
			return nil, problem(http.StatusConflict, "execution-id-conflict", "this execution id is already in use")
		} else if errors.Is(err, ErrInvalidChecklistItem) {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-checklist-item", "an item does not belong to this order's checklist version")
		} else if errors.Is(err, ErrExecutionItemConflict) {
			return nil, problem(http.StatusConflict, "execution-item-conflict", "an item id is already used by a different execution")
		} else if err != nil {
			return nil, err
		}
		view, err := loadExecutionView(ctx, db.New(pool), p.TenantID, execID)
		if err != nil {
			return nil, err
		}
		return &executionOutput{Body: view}, nil
	})
}

// === staff work orders ======================================================

const dateLayout = "2006-01-02"

type orderCreateBody struct {
	ObjectID   string  `json:"object_id" format:"uuid"`
	VersionID  string  `json:"version_id" format:"uuid"`
	AssigneeID *string `json:"assignee_id,omitempty" format:"uuid"`
	DueDate    string  `json:"due_date" example:"2026-07-13" doc:"YYYY-MM-DD"`
}

// bulkCreateOrdersInput.Body carries `minItems`/`maxItems` tags — huma
// applies these as JSON-schema validation and rejects a request with an
// empty or >100-item array (422) before the handler ever runs (see
// docs/features/request-inputs.md: "All doc & validation tags are allowed
// on the body"), so no separate length check is needed in the handler.
// A literal JSON `null` body, however, satisfies the generated
// ["array","null"] schema and skips minItems entirely, so the handler
// still guards against a nil Body explicitly (see the nil check below).
type bulkCreateOrdersInput struct {
	Body []orderCreateBody `minItems:"1" maxItems:"100"`
}

type bulkCreateOrdersOutput struct {
	Body struct {
		Created int         `json:"created"`
		IDs     []uuid.UUID `json:"ids"`
	}
}

type workOrderView struct {
	ID         uuid.UUID  `json:"id"`
	ObjectID   uuid.UUID  `json:"object_id"`
	VersionID  uuid.UUID  `json:"version_id"`
	AssigneeID *uuid.UUID `json:"assignee_id,omitempty"`
	DueDate    string     `json:"due_date"`
	Status     string     `json:"status"`
	CreatedAt  time.Time  `json:"created_at"`
}

// toWorkOrderView maps the common work_order row shape shared by
// InsertWorkOrder/GetWorkOrder/ListWorkOrders/UpdateWorkOrder — sqlc emits an
// identical field set for each as its own named Row type, so this takes the
// fields directly rather than one specific generated type.
func toWorkOrderView(id, objectID, versionID uuid.UUID, assigneeID uuid.NullUUID, dueDate pgtype.Date, status db.WorkOrderStatus, createdAt pgtype.Timestamptz) workOrderView {
	v := workOrderView{
		ID:        id,
		ObjectID:  objectID,
		VersionID: versionID,
		DueDate:   dueDate.Time.Format(dateLayout),
		Status:    string(status),
		CreatedAt: createdAt.Time,
	}
	if assigneeID.Valid {
		a := assigneeID.UUID
		v.AssigneeID = &a
	}
	return v
}

func uuidSetToSlice(set map[uuid.UUID]struct{}) []uuid.UUID {
	out := make([]uuid.UUID, 0, len(set))
	for id := range set {
		out = append(out, id)
	}
	return out
}

// validateOrderReferences checks that every distinct object/version/assignee
// id referenced by a bulk-create batch actually belongs to this tenant,
// comparing the distinct count submitted against the count found. A mismatch
// means at least one id is unknown or belongs to another tenant.
func validateOrderReferences(ctx context.Context, q *db.Queries, tenantID uuid.UUID, objectIDs, versionIDs, assigneeIDs map[uuid.UUID]struct{}) error {
	if len(objectIDs) > 0 {
		n, err := q.CountObjectsInTenant(ctx, db.CountObjectsInTenantParams{TenantID: tenantID, Ids: uuidSetToSlice(objectIDs)})
		if err != nil {
			return fmt.Errorf("counting objects: %w", err)
		}
		if int(n) != len(objectIDs) {
			return problem(http.StatusUnprocessableEntity, "invalid-reference", "one or more object_id values are unknown for this tenant")
		}
	}
	if len(versionIDs) > 0 {
		n, err := q.CountVersionsInTenant(ctx, db.CountVersionsInTenantParams{TenantID: tenantID, Ids: uuidSetToSlice(versionIDs)})
		if err != nil {
			return fmt.Errorf("counting checklist versions: %w", err)
		}
		if int(n) != len(versionIDs) {
			return problem(http.StatusUnprocessableEntity, "invalid-reference", "one or more version_id values are unknown for this tenant")
		}
	}
	if len(assigneeIDs) > 0 {
		n, err := q.CountActiveWorkersInTenant(ctx, db.CountActiveWorkersInTenantParams{TenantID: tenantID, Ids: uuidSetToSlice(assigneeIDs)})
		if err != nil {
			return fmt.Errorf("counting workers: %w", err)
		}
		if int(n) != len(assigneeIDs) {
			return problem(http.StatusUnprocessableEntity, "invalid-reference", "one or more assignee_id values are not active workers in this tenant")
		}
	}
	return nil
}

type parsedOrder struct {
	id         uuid.UUID
	objectID   uuid.UUID
	versionID  uuid.UUID
	assigneeID uuid.NullUUID
	dueDate    pgtype.Date
}

// parseOrderBatch parses and UUID/date-validates every item in a bulk-create
// request, and collects the distinct referenced ids for validateOrderReferences.
func parseOrderBatch(items []orderCreateBody) ([]parsedOrder, map[uuid.UUID]struct{}, map[uuid.UUID]struct{}, map[uuid.UUID]struct{}, error) {
	parsed := make([]parsedOrder, 0, len(items))
	objectIDs := map[uuid.UUID]struct{}{}
	versionIDs := map[uuid.UUID]struct{}{}
	assigneeIDs := map[uuid.UUID]struct{}{}
	for _, item := range items {
		objectID, err := uuid.Parse(item.ObjectID)
		if err != nil {
			return nil, nil, nil, nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid object_id")
		}
		versionID, err := uuid.Parse(item.VersionID)
		if err != nil {
			return nil, nil, nil, nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid version_id")
		}
		var assigneeID uuid.NullUUID
		if item.AssigneeID != nil {
			a, err := uuid.Parse(*item.AssigneeID)
			if err != nil {
				return nil, nil, nil, nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid assignee_id")
			}
			assigneeID = uuid.NullUUID{UUID: a, Valid: true}
			assigneeIDs[a] = struct{}{}
		}
		due, err := time.Parse(dateLayout, item.DueDate)
		if err != nil {
			return nil, nil, nil, nil, problem(http.StatusUnprocessableEntity, "invalid-date", "due_date must be YYYY-MM-DD")
		}
		objectIDs[objectID] = struct{}{}
		versionIDs[versionID] = struct{}{}
		parsed = append(parsed, parsedOrder{
			id: uuid.New(), objectID: objectID, versionID: versionID,
			assigneeID: assigneeID, dueDate: pgtype.Date{Time: due, Valid: true},
		})
	}
	return parsed, objectIDs, versionIDs, assigneeIDs, nil
}

type listOrdersInput struct {
	Date       string `query:"date" doc:"YYYY-MM-DD"`
	ObjectID   string `query:"object_id" format:"uuid"`
	AssigneeID string `query:"assignee_id" format:"uuid"`
	Status     string `query:"status"`
}

type listOrdersOutput struct {
	Body struct {
		WorkOrders []workOrderView `json:"work_orders"`
	}
}

type patchOrderBody struct {
	AssigneeID *string `json:"assignee_id,omitempty" format:"uuid"`
	DueDate    *string `json:"due_date,omitempty" example:"2026-07-20" doc:"YYYY-MM-DD"`
}

type patchOrderInput struct {
	ID   string `path:"id"`
	Body patchOrderBody
}

type orderOutput struct {
	Body workOrderView
}

// registerStaffOrders wires the staff-facing work-order bulk create / list /
// patch endpoints. Bulk create runs one transaction so a batch either lands
// entirely or not at all (see the bulk-atomicity test). List and patch are
// single-statement queries under the existing per-call db.Queries.
func registerStaffOrders(api huma.API, pool *pgxpool.Pool) {
	q := db.New(pool)

	huma.Register(api, huma.Operation{
		OperationID:   "createStaffWorkOrders",
		Method:        http.MethodPost,
		Path:          "/api/v1/staff/work-orders",
		Summary:       "Bulk-create work orders",
		Tags:          []string{"staff"},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *bulkCreateOrdersInput) (*bulkCreateOrdersOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}

		if in.Body == nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-reference", "request body must be a non-empty array of orders")
		}

		parsed, objectIDs, versionIDs, assigneeIDs, perr := parseOrderBatch(in.Body)
		if perr != nil {
			return nil, perr
		}
		if err := validateOrderReferences(ctx, q, principal.TenantID, objectIDs, versionIDs, assigneeIDs); err != nil {
			return nil, err
		}

		tx, err := pool.Begin(ctx)
		if err != nil {
			return nil, fmt.Errorf("begin tx: %w", err)
		}
		defer func() { _ = tx.Rollback(ctx) }()
		qtx := q.WithTx(tx)

		ids := make([]uuid.UUID, 0, len(parsed))
		for _, p := range parsed {
			if err := qtx.InsertWorkOrder(ctx, db.InsertWorkOrderParams{
				ID: p.id, TenantID: principal.TenantID, ObjectID: p.objectID,
				VersionID: p.versionID, AssigneeID: p.assigneeID, DueDate: p.dueDate,
			}); err != nil {
				return nil, fmt.Errorf("inserting work order: %w", err)
			}
			ids = append(ids, p.id)
		}
		if err := tx.Commit(ctx); err != nil {
			return nil, fmt.Errorf("commit: %w", err)
		}

		out := &bulkCreateOrdersOutput{}
		out.Body.Created = len(ids)
		out.Body.IDs = ids
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listStaffWorkOrders",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/work-orders",
		Summary:     "List work orders",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *listOrdersInput) (*listOrdersOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		params := db.ListWorkOrdersParams{TenantID: principal.TenantID}
		if in.Date != "" {
			d, err := time.Parse(dateLayout, in.Date)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-date", "date must be YYYY-MM-DD")
			}
			params.DueDate = pgtype.Date{Time: d, Valid: true}
		}
		if in.ObjectID != "" {
			oid, err := uuid.Parse(in.ObjectID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid object_id")
			}
			params.ObjectID = uuid.NullUUID{UUID: oid, Valid: true}
		}
		if in.AssigneeID != "" {
			aid, err := uuid.Parse(in.AssigneeID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid assignee_id")
			}
			params.AssigneeID = uuid.NullUUID{UUID: aid, Valid: true}
		}
		if in.Status != "" {
			st := db.WorkOrderStatus(in.Status)
			switch st {
			case db.WorkOrderStatusScheduled, db.WorkOrderStatusInProgress, db.WorkOrderStatusDone, db.WorkOrderStatusMissed:
				params.Status = &st
			default:
				return nil, problem(http.StatusUnprocessableEntity, "invalid-status", "unknown status value")
			}
		}

		rows, err := q.ListWorkOrders(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("listing work orders for tenant %s: %w", principal.TenantID, err)
		}
		out := &listOrdersOutput{}
		out.Body.WorkOrders = make([]workOrderView, 0, len(rows))
		for _, r := range rows {
			out.Body.WorkOrders = append(out.Body.WorkOrders, toWorkOrderView(r.ID, r.ObjectID, r.VersionID, r.AssigneeID, r.DueDate, r.Status, r.CreatedAt))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "patchStaffWorkOrder",
		Method:      http.MethodPatch,
		Path:        "/api/v1/staff/work-orders/{id}",
		Summary:     "Reassign or reschedule a work order",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *patchOrderInput) (*orderOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid work order id")
		}
		params := db.UpdateWorkOrderParams{ID: id, TenantID: principal.TenantID}
		if in.Body.AssigneeID != nil {
			aid, err := uuid.Parse(*in.Body.AssigneeID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid assignee_id")
			}
			n, err := q.CountActiveWorkersInTenant(ctx, db.CountActiveWorkersInTenantParams{TenantID: principal.TenantID, Ids: []uuid.UUID{aid}})
			if err != nil {
				return nil, fmt.Errorf("validating assignee %s: %w", aid, err)
			}
			if n != 1 {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-reference", "assignee_id is not an active worker in this tenant")
			}
			params.AssigneeID = uuid.NullUUID{UUID: aid, Valid: true}
		}
		if in.Body.DueDate != nil {
			d, err := time.Parse(dateLayout, *in.Body.DueDate)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-date", "due_date must be YYYY-MM-DD")
			}
			params.DueDate = pgtype.Date{Time: d, Valid: true}
		}

		row, err := q.UpdateWorkOrder(ctx, params)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "work-order-not-found", "no work order with this id")
		}
		if err != nil {
			return nil, fmt.Errorf("updating work order %s: %w", id, err)
		}
		return &orderOutput{Body: toWorkOrderView(row.ID, row.ObjectID, row.VersionID, row.AssigneeID, row.DueDate, row.Status, row.CreatedAt)}, nil
	})
}

// === staff evidence reads ===================================================

type staffExecutionItemView struct {
	ID             uuid.UUID  `json:"id"`
	TemplateItemID uuid.UUID  `json:"template_item_id"`
	Position       int32      `json:"position"`
	Title          string     `json:"title"`
	Checked        bool       `json:"checked"`
	CheckedAt      *time.Time `json:"checked_at,omitempty"`
}

// staffExecutionPhotoView never drops an unconfirmed photo: Uploaded is
// always present, and URL/URLExpiresAt are only populated (pointer +
// omitempty) once the photo has actually landed in S3 — evidence, confirmed
// or not, stays visible.
type staffExecutionPhotoView struct {
	ID           uuid.UUID  `json:"id"`
	Kind         string     `json:"kind"`
	TakenAt      *time.Time `json:"taken_at,omitempty"`
	Lat          *float64   `json:"lat,omitempty"`
	Lon          *float64   `json:"lon,omitempty"`
	Uploaded     bool       `json:"uploaded"`
	URL          *string    `json:"url,omitempty"`
	URLExpiresAt *time.Time `json:"url_expires_at,omitempty"`
}

type staffExecutionDetailView struct {
	ID               uuid.UUID                 `json:"id"`
	WorkOrderID      uuid.UUID                 `json:"work_order_id"`
	ObjectID         uuid.UUID                 `json:"object_id"`
	ObjectName       string                    `json:"object_name"`
	WorkerName       string                    `json:"worker_name"`
	CreatedAt        time.Time                 `json:"created_at"`
	StartedAt        *time.Time                `json:"started_at,omitempty"`
	FinishedAt       *time.Time                `json:"finished_at,omitempty"`
	DeviceFinishedAt *time.Time                `json:"device_finished_at,omitempty"`
	Note             *string                   `json:"note,omitempty"`
	Items            []staffExecutionItemView  `json:"items"`
	Photos           []staffExecutionPhotoView `json:"photos"`
}

type staffExecutionDetailInput struct {
	ID string `path:"id"`
}

type staffExecutionDetailOutput struct {
	Body staffExecutionDetailView
}

// registerStaffExecutionDetail wires GET /api/v1/staff/executions/{id}: the
// checklist items (with titles) and photo evidence for one execution, with a
// presigned GET URL for every confirmed photo.
//
// GetExecutionDetail is the only tenant-scoped lookup of the three queries
// this handler runs — ListExecutionItemsWithTitles and ListExecutionPhotos
// both take a bare executionID with no tenant filter. GetExecutionDetail
// MUST run first and 404 on a miss before either of those queries touches
// the id, otherwise a staff principal from tenant A could read tenant B's
// checklist items and photos for a guessed/enumerated execution id.
func registerStaffExecutionDetail(api huma.API, q *db.Queries, store photo.ObjectStore) {
	huma.Register(api, huma.Operation{
		OperationID: "getStaffExecution",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/executions/{id}",
		Summary:     "Execution detail: checklist items and photo evidence",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *staffExecutionDetailInput) (*staffExecutionDetailOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		execID, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid execution id")
		}
		e, err := q.GetExecutionDetail(ctx, db.GetExecutionDetailParams{ID: execID, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "execution-not-found", "execution not found")
		}
		if err != nil {
			return nil, fmt.Errorf("loading execution detail: %w", err)
		}

		items, err := q.ListExecutionItemsWithTitles(ctx, execID)
		if err != nil {
			return nil, fmt.Errorf("loading execution items: %w", err)
		}
		photos, err := q.ListExecutionPhotos(ctx, uuid.NullUUID{UUID: execID, Valid: true})
		if err != nil {
			return nil, fmt.Errorf("loading execution photos: %w", err)
		}

		view := staffExecutionDetailView{
			ID: e.ID, WorkOrderID: e.WorkOrderID, ObjectID: e.ObjectID, ObjectName: e.ObjectName,
			WorkerName: e.WorkerName, CreatedAt: e.CreatedAt.Time,
			StartedAt: tptr(e.StartedAt), FinishedAt: tptr(e.FinishedAt), DeviceFinishedAt: tptr(e.DeviceFinishedAt),
			Note:   e.Note,
			Items:  make([]staffExecutionItemView, 0, len(items)),
			Photos: make([]staffExecutionPhotoView, 0, len(photos)),
		}
		for _, it := range items {
			view.Items = append(view.Items, staffExecutionItemView{
				ID: it.ID, TemplateItemID: it.TemplateItemID, Position: it.Position, Title: it.Title,
				Checked: it.Checked, CheckedAt: tptr(it.CheckedAt),
			})
		}
		for _, ph := range photos {
			pv := staffExecutionPhotoView{
				ID: ph.ID, Kind: string(ph.Kind), TakenAt: tptr(ph.TakenAt), Lat: ph.Lat, Lon: ph.Lon,
				Uploaded: ph.UploadedAt.Valid,
			}
			// An unconfirmed photo (uploaded_at still null) stays in the list
			// with uploaded:false and no url — evidence is never silently
			// dropped just because the upload hasn't landed yet.
			if ph.UploadedAt.Valid {
				url, expires, err := store.PresignGet(ctx, ph.S3Key)
				if err != nil {
					return nil, fmt.Errorf("presigning photo %s: %w", ph.ID, err)
				}
				pv.URL = &url
				pv.URLExpiresAt = &expires
			}
			view.Photos = append(view.Photos, pv)
		}
		return &staffExecutionDetailOutput{Body: view}, nil
	})
}

// === staff dashboard =========================================================

type dashboardInput struct {
	Date string `query:"date" doc:"YYYY-MM-DD; defaults to tenant-local today"`
}

type dashboardTotals struct {
	Done       int `json:"done"`
	InProgress int `json:"in_progress"`
	Overdue    int `json:"overdue"`
	Total      int `json:"total"`
}

type dashboardObjectView struct {
	ObjectID       uuid.UUID  `json:"object_id"`
	ObjectName     string     `json:"object_name"`
	Address        *string    `json:"address,omitempty"`
	Lat            *float64   `json:"lat,omitempty"`
	Lon            *float64   `json:"lon,omitempty"`
	OrderID        uuid.UUID  `json:"order_id"`
	Status         string     `json:"status"`
	WorkerName     *string    `json:"worker_name,omitempty"`
	LastActivityAt *time.Time `json:"last_activity_at,omitempty"`
	LastFinishedAt *time.Time `json:"last_finished_at,omitempty"`
	PhotoCount     int64      `json:"photo_count"`
}

type dashboardOutput struct {
	Body struct {
		Date    string                `json:"date"`
		Totals  dashboardTotals       `json:"totals"`
		Objects []dashboardObjectView `json:"objects"`
	}
}

// registerStaffDashboard wires GET /api/v1/staff/dashboard: per-object today
// (or a requested date) statuses plus roll-up totals.
//
// "Overdue" is always judged against tenant-local *today* (fetched via
// TenantToday regardless of whether a date was requested), not the queried
// date — viewing a past date's dashboard should surface which of its
// scheduled/in_progress orders would now be overdue (the phase-6 nightly job
// converts those to `missed`), and viewing today always yields overdue:0
// since due_date == tenant-today by construction.
func registerStaffDashboard(api huma.API, q *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "getStaffDashboard",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/dashboard",
		Summary:     "Per-object statuses and totals for a day",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *dashboardInput) (*dashboardOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		tenantToday, err := TenantToday(ctx, q, p.TenantID)
		if err != nil {
			return nil, fmt.Errorf("computing tenant-local today for tenant %s: %w", p.TenantID, err)
		}
		queryDate := tenantToday
		if in.Date != "" {
			d, err := time.Parse(dateLayout, in.Date)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-date", "date must be YYYY-MM-DD")
			}
			queryDate = pgtype.Date{Time: d, Valid: true}
		}

		rows, err := q.ListDashboardOrders(ctx, db.ListDashboardOrdersParams{TenantID: p.TenantID, DueDate: queryDate})
		if err != nil {
			return nil, fmt.Errorf("listing dashboard orders for tenant %s: %w", p.TenantID, err)
		}

		out := &dashboardOutput{}
		out.Body.Date = queryDate.Time.Format(dateLayout)
		out.Body.Objects = make([]dashboardObjectView, 0, len(rows))
		for _, r := range rows {
			switch r.Status {
			case db.WorkOrderStatusDone:
				out.Body.Totals.Done++
			case db.WorkOrderStatusInProgress:
				out.Body.Totals.InProgress++
			}
			if (r.Status == db.WorkOrderStatusScheduled || r.Status == db.WorkOrderStatusInProgress) && r.DueDate.Time.Before(tenantToday.Time) {
				out.Body.Totals.Overdue++
			}
			out.Body.Objects = append(out.Body.Objects, dashboardObjectView{
				ObjectID: r.ObjectID, ObjectName: r.ObjectName, Address: r.Address, Lat: r.Lat, Lon: r.Lon,
				OrderID: r.OrderID, Status: string(r.Status), WorkerName: r.WorkerName,
				LastActivityAt: tptr(r.LastActivityAt), LastFinishedAt: tptr(r.LastFinishedAt),
				PhotoCount: r.PhotoCount,
			})
		}
		out.Body.Totals.Total = len(rows)
		return out, nil
	})
}
