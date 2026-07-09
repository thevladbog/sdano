// Package object exposes serviced-object endpoints. The list endpoint is the
// walking-skeleton tracer proving SQL → sqlc → huma → OpenAPI end to end.
package object

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

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
}
