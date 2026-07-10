// Package workorder exposes the worker-facing planned-loop endpoints: the
// today bootstrap read and the idempotent execution upsert.
package workorder

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

// Register wires the worker planned-loop routes onto api. It takes the pool
// (not just Queries) because the execution upsert added in Task 3 runs a
// transaction. Route registration never touches the pool until a request runs,
// so a nil pool (openapi mode) is fine.
func Register(api huma.API, pool *pgxpool.Pool) {
	q := db.New(pool)
	registerToday(api, q)
	registerExecutions(api, pool)
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
		orders, err := q.ListWorkerTodayOrders(ctx, db.ListWorkerTodayOrdersParams{
			TenantID:   p.TenantID,
			AssigneeID: uuid.NullUUID{UUID: p.UserID, Valid: true},
			DueDate:    pgtype.Date{Time: now, Valid: true},
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
