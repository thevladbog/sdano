// Package object exposes serviced-object endpoints. The list endpoint is the
// walking-skeleton tracer proving SQL → sqlc → huma → OpenAPI end to end.
package object

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
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

	registerWorkerQR(api, queries)
}

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
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
		TodayWorkOrder *qrTodayOrder `json:"today_work_order"`
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
		wo, err := queries.GetWorkerOrderForObject(ctx, db.GetWorkerOrderForObjectParams{
			TenantID: p.TenantID, AssigneeID: uuid.NullUUID{UUID: p.UserID, Valid: true}, ObjectID: obj.ID,
			DueDate: pgtype.Date{Time: time.Now().UTC(), Valid: true},
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
