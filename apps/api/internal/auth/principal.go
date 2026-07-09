// Package auth handles authentication and authorization: password hashing,
// access/refresh/device tokens, the auth service, and the request middleware
// chain. It defines the authenticated Principal that middleware establishes
// and handlers read.
package auth

import (
	"context"

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
