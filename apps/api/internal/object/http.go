// Package object exposes serviced-object endpoints. The list endpoint is the
// walking-skeleton tracer proving SQL → sqlc → huma → OpenAPI end to end.
package object

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/workorder"
)

type Object struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name" example:"Lenina st., 45 — bus stop"`
	Address   *string   `json:"address"`
	Lat       *float64  `json:"lat"`
	Lon       *float64  `json:"lon"`
	Kind      *string   `json:"kind" example:"bus_stop"`
	QRToken   *string   `json:"qr_token"`
	IsActive  bool      `json:"is_active"`
	CreatedAt time.Time `json:"created_at"`
}

type listOutput struct {
	Body struct {
		Objects []Object `json:"objects"`
	}
}

func Register(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "listStaffObjects",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/objects",
		Summary:     "List active objects",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, _ *struct{}) (*listOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		rows, err := queries.ListObjects(ctx, principal.TenantID)
		if err != nil {
			return nil, fmt.Errorf("listing objects for tenant %s: %w", principal.TenantID, err)
		}
		out := &listOutput{}
		out.Body.Objects = make([]Object, 0, len(rows))
		for _, r := range rows {
			out.Body.Objects = append(out.Body.Objects, Object{
				ID:        r.ID,
				Name:      r.Name,
				Address:   r.Address,
				Lat:       r.Lat,
				Lon:       r.Lon,
				Kind:      r.Kind,
				QRToken:   r.QrToken,
				IsActive:  r.IsActive,
				CreatedAt: r.CreatedAt.Time,
			})
		}
		return out, nil
	})

	registerStaffObjectWrites(api, queries)
	registerStaffObjectReads(api, queries)
	registerWorkerQR(api, queries)
}

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

// === staff object writes ===================================================

type objectBody struct {
	Name       string     `json:"name" minLength:"1"`
	Address    *string    `json:"address,omitempty"`
	Lat        *float64   `json:"lat,omitempty"`
	Lon        *float64   `json:"lon,omitempty"`
	Kind       *string    `json:"kind,omitempty"`
	QRToken    *string    `json:"qr_token,omitempty"`
	ContractID *uuid.UUID `json:"contract_id,omitempty"`
}

type objectPatchBody struct {
	Name       *string    `json:"name,omitempty"`
	Address    *string    `json:"address,omitempty"`
	Lat        *float64   `json:"lat,omitempty"`
	Lon        *float64   `json:"lon,omitempty"`
	Kind       *string    `json:"kind,omitempty"`
	QRToken    *string    `json:"qr_token,omitempty"`
	ContractID *uuid.UUID `json:"contract_id,omitempty"`
	IsActive   *bool      `json:"is_active,omitempty"`
}

type createObjectInput struct {
	Body objectBody
}

type patchObjectInput struct {
	ID   string `path:"id"`
	Body objectPatchBody
}

type objectOutput struct {
	Body Object
}

// nullUUID lifts an optional UUID into the uuid.NullUUID sqlc expects for a
// nullable column, without dereferencing a nil pointer.
func nullUUID(id *uuid.UUID) uuid.NullUUID {
	if id == nil {
		return uuid.NullUUID{}
	}
	return uuid.NullUUID{UUID: *id, Valid: true}
}

func registerStaffObjectWrites(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID:   "createStaffObject",
		Method:        http.MethodPost,
		Path:          "/api/v1/staff/objects",
		Summary:       "Create an object",
		Tags:          []string{"staff"},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createObjectInput) (*objectOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		row, err := queries.InsertObject(ctx, db.InsertObjectParams{
			TenantID:   principal.TenantID,
			Name:       in.Body.Name,
			Address:    in.Body.Address,
			Lat:        in.Body.Lat,
			Lon:        in.Body.Lon,
			Kind:       in.Body.Kind,
			QrToken:    in.Body.QRToken,
			ContractID: nullUUID(in.Body.ContractID),
		})
		if err != nil {
			return nil, fmt.Errorf("inserting object for tenant %s: %w", principal.TenantID, err)
		}
		return &objectOutput{Body: Object{
			ID: row.ID, Name: row.Name, Address: row.Address, Lat: row.Lat, Lon: row.Lon,
			Kind: row.Kind, QRToken: row.QrToken, IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "patchStaffObject",
		Method:      http.MethodPatch,
		Path:        "/api/v1/staff/objects/{id}",
		Summary:     "Update an object",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *patchObjectInput) (*objectOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid object id")
		}
		row, err := queries.UpdateObject(ctx, db.UpdateObjectParams{
			Name:       in.Body.Name,
			Address:    in.Body.Address,
			Lat:        in.Body.Lat,
			Lon:        in.Body.Lon,
			Kind:       in.Body.Kind,
			QrToken:    in.Body.QRToken,
			ContractID: nullUUID(in.Body.ContractID),
			IsActive:   in.Body.IsActive,
			ID:         id,
			TenantID:   principal.TenantID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "object-not-found", "no object with this id")
		}
		if err != nil {
			return nil, fmt.Errorf("updating object %s: %w", id, err)
		}
		return &objectOutput{Body: Object{
			ID: row.ID, Name: row.Name, Address: row.Address, Lat: row.Lat, Lon: row.Lon,
			Kind: row.Kind, QRToken: row.QrToken, IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
		}}, nil
	})
}

// === staff object reads: card + execution history ==========================

// maxCursorID is the sentinel "after" id paired with a far-future timestamp
// to select the first page of a (created_at, id) DESC keyset without a
// special-cased query variant.
var maxCursorID = uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")

// firstPageCursor returns the keyset sentinel for the first page: a moment
// guaranteed to be after any real row's created_at, so `(created_at, id) <
// (sentinel, maxCursorID)` matches everything.
func firstPageCursor() (pgtype.Timestamptz, uuid.UUID) {
	return pgtype.Timestamptz{Time: time.Now().Add(24 * time.Hour), Valid: true}, maxCursorID
}

func encodeCursor(t time.Time, id uuid.UUID) string {
	return base64.RawURLEncoding.EncodeToString([]byte(t.UTC().Format(time.RFC3339Nano) + "|" + id.String()))
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("decoding cursor: %w", err)
	}
	parts := strings.SplitN(string(raw), "|", 2)
	if len(parts) != 2 {
		return time.Time{}, uuid.Nil, errors.New("malformed cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor time: %w", err)
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return time.Time{}, uuid.Nil, fmt.Errorf("cursor id: %w", err)
	}
	return ts, id, nil
}

// tsPtr converts a possibly-unset pgtype.Timestamptz into a *time.Time, nil
// when the column is SQL NULL (execution not yet started/finished).
func tsPtr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

type objectExecutionView struct {
	ID               uuid.UUID  `json:"id"`
	WorkOrderID      uuid.UUID  `json:"work_order_id"`
	CreatedAt        time.Time  `json:"created_at"`
	StartedAt        *time.Time `json:"started_at,omitempty"`
	FinishedAt       *time.Time `json:"finished_at,omitempty"`
	DeviceFinishedAt *time.Time `json:"device_finished_at,omitempty"`
	WorkerName       string     `json:"worker_name"`
	PhotoCount       int64      `json:"photo_count"`
}

func toExecutionView(r db.ListObjectExecutionsRow) objectExecutionView {
	return objectExecutionView{
		ID:               r.ID,
		WorkOrderID:      r.WorkOrderID,
		CreatedAt:        r.CreatedAt.Time,
		StartedAt:        tsPtr(r.StartedAt),
		FinishedAt:       tsPtr(r.FinishedAt),
		DeviceFinishedAt: tsPtr(r.DeviceFinishedAt),
		WorkerName:       r.WorkerName,
		PhotoCount:       r.PhotoCount,
	}
}

type objectCardInput struct {
	ID string `path:"id"`
}

type objectCardOutput struct {
	Body struct {
		Object           Object                `json:"object"`
		RecentExecutions []objectExecutionView `json:"recent_executions"`
	}
}

type listExecutionsInput struct {
	ID     string `path:"id"`
	Cursor string `query:"cursor"`
	Limit  int    `query:"limit"`
}

type listExecutionsOutput struct {
	Body struct {
		Executions []objectExecutionView `json:"executions"`
		NextCursor string                `json:"next_cursor,omitempty"`
	}
}

func registerStaffObjectReads(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "getStaffObject",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/objects/{id}",
		Summary:     "Object card: details plus recent executions",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *objectCardInput) (*objectCardOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid object id")
		}
		obj, err := queries.GetObject(ctx, db.GetObjectParams{ID: id, TenantID: principal.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "object-not-found", "no object with this id")
		}
		if err != nil {
			return nil, fmt.Errorf("loading object %s: %w", id, err)
		}
		afterCreatedAt, afterID := firstPageCursor()
		rows, err := queries.ListObjectExecutions(ctx, db.ListObjectExecutionsParams{
			TenantID:       principal.TenantID,
			ObjectID:       id,
			AfterCreatedAt: afterCreatedAt,
			AfterID:        afterID,
			PageLimit:      10,
		})
		if err != nil {
			return nil, fmt.Errorf("listing recent executions for object %s: %w", id, err)
		}
		out := &objectCardOutput{}
		out.Body.Object = Object{
			ID: obj.ID, Name: obj.Name, Address: obj.Address, Lat: obj.Lat, Lon: obj.Lon,
			Kind: obj.Kind, QRToken: obj.QrToken, IsActive: obj.IsActive, CreatedAt: obj.CreatedAt.Time,
		}
		out.Body.RecentExecutions = make([]objectExecutionView, 0, len(rows))
		for _, r := range rows {
			out.Body.RecentExecutions = append(out.Body.RecentExecutions, toExecutionView(r))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "listStaffObjectExecutions",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/objects/{id}/executions",
		Summary:     "Object execution history (cursor-paginated)",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *listExecutionsInput) (*listExecutionsOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid object id")
		}
		if _, err := queries.GetObject(ctx, db.GetObjectParams{ID: id, TenantID: principal.TenantID}); errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "object-not-found", "no object with this id")
		} else if err != nil {
			return nil, fmt.Errorf("loading object %s: %w", id, err)
		}

		limit := in.Limit
		switch {
		case limit <= 0:
			limit = 20
		case limit > 100:
			limit = 100
		}

		afterCreatedAt, afterID := firstPageCursor()
		if in.Cursor != "" {
			t, cid, cerr := decodeCursor(in.Cursor)
			if cerr != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-cursor", "cursor is malformed")
			}
			afterCreatedAt, afterID = pgtype.Timestamptz{Time: t, Valid: true}, cid
		}

		rows, err := queries.ListObjectExecutions(ctx, db.ListObjectExecutionsParams{
			TenantID:       principal.TenantID,
			ObjectID:       id,
			AfterCreatedAt: afterCreatedAt,
			AfterID:        afterID,
			PageLimit:      int32(limit),
		})
		if err != nil {
			return nil, fmt.Errorf("listing executions for object %s: %w", id, err)
		}

		out := &listExecutionsOutput{}
		out.Body.Executions = make([]objectExecutionView, 0, len(rows))
		for _, r := range rows {
			out.Body.Executions = append(out.Body.Executions, toExecutionView(r))
		}
		if len(rows) == limit {
			last := rows[len(rows)-1]
			out.Body.NextCursor = encodeCursor(last.CreatedAt.Time, last.ID)
		}
		return out, nil
	})
}

type qrObject struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Address  *string   `json:"address"`
	Lat      *float64  `json:"lat"`
	Lon      *float64  `json:"lon"`
	Kind     *string   `json:"kind"`
	QRToken  *string   `json:"qr_token"`
	IsActive bool      `json:"is_active"`
}

type qrTodayOrder struct {
	ID        uuid.UUID `json:"id"`
	ObjectID  uuid.UUID `json:"object_id"`
	DueDate   string    `json:"due_date"`
	Status    string    `json:"status"`
	VersionID uuid.UUID `json:"version_id"`
}

type qrInput struct {
	QRToken string `path:"qr_token"`
}

type qrOutput struct {
	Body struct {
		Object         qrObject      `json:"object"`
		TodayWorkOrder *qrTodayOrder `json:"today_work_order,omitempty"`
	}
}

func registerWorkerQR(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "workerObjectByQR",
		Method:      http.MethodGet,
		Path:        "/api/v1/worker/objects/by-qr/{qr_token}",
		Summary:     "Resolve an object by its QR token",
		Tags:        []string{"worker"},
	}, func(ctx context.Context, in *qrInput) (*qrOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		token := in.QRToken
		obj, err := queries.GetObjectByQr(ctx, db.GetObjectByQrParams{TenantID: p.TenantID, QrToken: &token})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "qr-not-found", "no object for this QR token")
		}
		if err != nil {
			return nil, fmt.Errorf("resolving qr: %w", err)
		}
		out := &qrOutput{}
		out.Body.Object = qrObject{
			ID: obj.ID, Name: obj.Name, Address: obj.Address, Lat: obj.Lat, Lon: obj.Lon,
			Kind: obj.Kind, QRToken: obj.QrToken, IsActive: obj.IsActive,
		}
		today, err := workorder.TenantToday(ctx, queries, p.TenantID)
		if err != nil {
			return nil, fmt.Errorf("computing tenant-local today for tenant %s: %w", p.TenantID, err)
		}
		wo, err := queries.GetWorkerOrderForObject(ctx, db.GetWorkerOrderForObjectParams{
			TenantID: p.TenantID, AssigneeID: uuid.NullUUID{UUID: p.UserID, Valid: true}, ObjectID: obj.ID,
			DueDate: today,
		})
		if err == nil {
			out.Body.TodayWorkOrder = &qrTodayOrder{
				ID: wo.ID, ObjectID: wo.ObjectID, DueDate: wo.DueDate.Time.Format("2006-01-02"),
				Status: string(wo.Status), VersionID: wo.VersionID,
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolving today order: %w", err)
		}
		return out, nil
	})
}
