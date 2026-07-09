// Package workorder exposes the worker-facing planned-loop endpoints: the
// today bootstrap read and the idempotent execution upsert.
package workorder

import (
	"context"
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
			items, err := q.ListChecklistItemsByVersions(ctx, versionIDs)
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
