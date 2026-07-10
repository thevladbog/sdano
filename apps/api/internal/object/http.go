// Package object exposes serviced-object endpoints. The list endpoint is the
// walking-skeleton tracer proving SQL → sqlc → huma → OpenAPI end to end.
package object

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/workorder"
)

type Object struct {
	ID         uuid.UUID  `json:"id"`
	Name       string     `json:"name" example:"Lenina st., 45 — bus stop"`
	Address    *string    `json:"address"`
	Lat        *float64   `json:"lat"`
	Lon        *float64   `json:"lon"`
	Kind       *string    `json:"kind" example:"bus_stop"`
	QRToken    *string    `json:"qr_token"`
	ContractID *uuid.UUID `json:"contract_id"`
	IsActive   bool       `json:"is_active"`
	CreatedAt  time.Time  `json:"created_at"`
}

type listOutput struct {
	Body struct {
		Objects []Object `json:"objects"`
	}
}

// listObjectsInput's filters are both optional: unfiltered returns every
// object in the tenant (active and inactive alike) — callers that want only
// live objects must say so explicitly with ?active=true. huma v2 panics on
// pointer-typed form/header/path/query fields ("pointers are not supported"),
// so — like Date/ObjectID/AssigneeID/Status in workorder's listOrdersInput —
// these are plain strings, empty meaning "not sent", parsed by hand below.
type listObjectsInput struct {
	Active     string `query:"active" doc:"true or false; omit for no filter"`
	ContractID string `query:"contract_id" format:"uuid"`
}

func Register(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "listStaffObjects",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/objects",
		Summary:     "List objects, optionally filtered by contract/active",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *listObjectsInput) (*listOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		params := db.ListObjectsParams{TenantID: principal.TenantID}
		if in.Active != "" {
			active, err := strconv.ParseBool(in.Active)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-active", "active must be true or false")
			}
			params.Active = &active
		}
		if in.ContractID != "" {
			cid, err := uuid.Parse(in.ContractID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-reference", "invalid contract_id")
			}
			params.ContractID = uuid.NullUUID{UUID: cid, Valid: true}
		}
		rows, err := queries.ListObjects(ctx, params)
		if err != nil {
			return nil, fmt.Errorf("listing objects for tenant %s: %w", principal.TenantID, err)
		}
		out := &listOutput{}
		out.Body.Objects = make([]Object, 0, len(rows))
		for _, r := range rows {
			out.Body.Objects = append(out.Body.Objects, Object{
				ID:         r.ID,
				Name:       r.Name,
				Address:    r.Address,
				Lat:        r.Lat,
				Lon:        r.Lon,
				Kind:       r.Kind,
				QRToken:    r.QrToken,
				ContractID: uuidPtr(r.ContractID),
				IsActive:   r.IsActive,
				CreatedAt:  r.CreatedAt.Time,
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

// patchObjectInput.Body is a pointer: every field is optional, so the body as
// a whole is too (huma marks any non-pointer Body required in the schema and
// rejects requests without one). A missing/empty body is a valid no-op patch.
type patchObjectInput struct {
	ID   string `path:"id"`
	Body *objectPatchBody
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

// uuidPtr is nullUUID's inverse for reads: a nullable UUID column becomes a
// *uuid.UUID, nil when the column is SQL NULL (no contract linked).
func uuidPtr(v uuid.NullUUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	u := v.UUID
	return &u
}

// validateContractRef checks that a caller-supplied contract_id belongs to
// this tenant, returning a 422 invalid-reference problem when it does not —
// covering both a cross-tenant contract and one that doesn't exist at all.
// A nil contractID (field omitted) is not validated.
func validateContractRef(ctx context.Context, queries *db.Queries, tenantID uuid.UUID, contractID *uuid.UUID) error {
	if contractID == nil {
		return nil
	}
	n, err := queries.CountContractsInTenant(ctx, db.CountContractsInTenantParams{TenantID: tenantID, Ids: []uuid.UUID{*contractID}})
	if err != nil {
		return fmt.Errorf("validating contract %s: %w", *contractID, err)
	}
	if n != 1 {
		return problem(http.StatusUnprocessableEntity, "invalid-reference", "contract does not belong to this tenant")
	}
	return nil
}

// qrConflict maps a unique_violation on object.qr_token to a 409 problem; it
// returns nil for any other error (including a nil err), letting the caller
// fall through to its own error handling.
func qrConflict(err error) *huma.ErrorModel {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return problem(http.StatusConflict, "qr-token-taken", "this QR token is already assigned to another object")
	}
	return nil
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
		if err := validateContractRef(ctx, queries, principal.TenantID, in.Body.ContractID); err != nil {
			return nil, err
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
		if p := qrConflict(err); p != nil {
			return nil, p
		}
		if err != nil {
			return nil, fmt.Errorf("inserting object for tenant %s: %w", principal.TenantID, err)
		}
		return &objectOutput{Body: Object{
			ID: row.ID, Name: row.Name, Address: row.Address, Lat: row.Lat, Lon: row.Lon,
			Kind: row.Kind, QRToken: row.QrToken, ContractID: uuidPtr(row.ContractID),
			IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
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
		body := objectPatchBody{}
		if in.Body != nil {
			body = *in.Body
		}
		if err := validateContractRef(ctx, queries, principal.TenantID, body.ContractID); err != nil {
			return nil, err
		}
		row, err := queries.UpdateObject(ctx, db.UpdateObjectParams{
			Name:       body.Name,
			Address:    body.Address,
			Lat:        body.Lat,
			Lon:        body.Lon,
			Kind:       body.Kind,
			QrToken:    body.QRToken,
			ContractID: nullUUID(body.ContractID),
			IsActive:   body.IsActive,
			ID:         id,
			TenantID:   principal.TenantID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "object-not-found", "no object with this id")
		}
		if p := qrConflict(err); p != nil {
			return nil, p
		}
		if err != nil {
			return nil, fmt.Errorf("updating object %s: %w", id, err)
		}
		return &objectOutput{Body: Object{
			ID: row.ID, Name: row.Name, Address: row.Address, Lat: row.Lat, Lon: row.Lon,
			Kind: row.Kind, QRToken: row.QrToken, ContractID: uuidPtr(row.ContractID),
			IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
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
			Kind: obj.Kind, QRToken: obj.QrToken, ContractID: uuidPtr(obj.ContractID),
			IsActive: obj.IsActive, CreatedAt: obj.CreatedAt.Time,
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
