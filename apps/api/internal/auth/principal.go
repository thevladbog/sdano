// Package auth defines the authenticated principal and the middleware that
// establishes it. The walking skeleton ships ONLY the dev header
// authenticator; the auth plan replaces it with JWT + device tokens.
package auth

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleWorker  Role = "worker"
)

type Principal struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     Role
}

type ctxKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal established by middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// NewDevTenantHeader authenticates via the X-Dev-Tenant-Id header.
// DEV ONLY (gated by DEV_TENANT_HEADER_AUTH): exists so the walking skeleton
// can exercise tenant-scoped queries before real auth lands. The auth plan
// deletes this middleware and the env flag together.
func NewDevTenantHeader(api huma.API, enabled bool) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if ctx.Operation().OperationID == "healthz" {
			next(ctx)
			return
		}
		if !enabled {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		tenantID, err := uuid.Parse(ctx.Header("X-Dev-Tenant-Id"))
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		next(huma.WithContext(ctx, withPrincipal(ctx.Context(), Principal{
			TenantID: tenantID,
			Role:     RoleAdmin,
		})))
	}
}
