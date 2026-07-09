package auth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/danielgtaylor/huma/v2"
	"github.com/jackc/pgx/v5"

	"sdano.app/api/internal/db"
)

const (
	metaPublic            = "auth.public"
	metaSuspendedWritable = "auth.suspended_writable"
)

// Public marks an operation as skipping authentication/authorization
// (healthz, the /auth/* endpoints).
func Public() map[string]any { return map[string]any{metaPublic: true} }

// SuspendedWritable marks a mutation that stays allowed while the tenant is
// suspended (evidence of pre-suspension work — see docs/12). Unused until the
// worker-API plan opts the execution-upsert endpoint in.
func SuspendedWritable() map[string]any { return map[string]any{metaSuspendedWritable: true} }

type Authenticator struct {
	secret string
	q      *db.Queries
}

func NewAuthenticator(secret string, q *db.Queries) *Authenticator {
	return &Authenticator{secret: secret, q: q}
}

// Authenticate resolves the bearer token to a Principal and stores it in the
// context. Public operations are skipped.
func (a *Authenticator) Authenticate(ctx huma.Context, next func(huma.Context)) {
	if isPublic(ctx.Operation()) {
		next(ctx)
		return
	}
	raw := bearer(ctx)
	if raw == "" {
		writeProblem(ctx, http.StatusUnauthorized, "authentication-required", "missing bearer token")
		return
	}
	p, err := a.resolve(ctx.Context(), raw)
	if err != nil {
		writeProblem(ctx, http.StatusUnauthorized, "authentication-required", "invalid token")
		return
	}
	next(huma.WithContext(ctx, withPrincipal(ctx.Context(), p)))
}

// Authorize enforces the role gate (by path prefix) and the tenant-status gate.
func (a *Authenticator) Authorize(ctx huma.Context, next func(huma.Context)) {
	if isPublic(ctx.Operation()) {
		next(ctx)
		return
	}
	p, ok := PrincipalFrom(ctx.Context())
	if !ok {
		writeProblem(ctx, http.StatusUnauthorized, "authentication-required", "no principal")
		return
	}
	path := ctx.Operation().Path
	switch {
	case strings.HasPrefix(path, "/api/v1/staff/"):
		if p.Role != RoleAdmin && p.Role != RoleManager {
			writeProblem(ctx, http.StatusForbidden, "forbidden-role", "staff role required")
			return
		}
	case strings.HasPrefix(path, "/api/v1/worker/"):
		if p.Role != RoleWorker {
			writeProblem(ctx, http.StatusForbidden, "forbidden-role", "worker role required")
			return
		}
	default:
		writeProblem(ctx, http.StatusForbidden, "forbidden-role", "no role gate configured for this path")
		return
	}

	status, err := a.q.GetTenantStatus(ctx.Context(), p.TenantID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			writeProblem(ctx, http.StatusUnauthorized, "authentication-required", "unknown tenant")
		} else {
			writeProblem(ctx, http.StatusInternalServerError, "internal-error", "unable to verify tenant status")
		}
		return
	}
	switch status {
	case db.TenantStatusArchived:
		writeProblem(ctx, http.StatusUnauthorized, "tenant-archived", "tenant archived")
		return
	case db.TenantStatusSuspended:
		if isMutation(ctx) && !suspendedWritable(ctx.Operation()) {
			writeProblem(ctx, http.StatusForbidden, "tenant-suspended", "tenant suspended; read-only access")
			return
		}
	}
	next(ctx)
}

func (a *Authenticator) resolve(ctx context.Context, raw string) (Principal, error) {
	if strings.Count(raw, ".") == 2 { // JWT: header.payload.signature
		return ParseAccessToken(a.secret, raw)
	}
	sess, err := a.q.GetDeviceSession(ctx, HashOpaqueToken(raw))
	if err != nil {
		return Principal{}, fmt.Errorf("device session: %w", err)
	}
	return Principal{UserID: sess.UserID, TenantID: sess.TenantID, Role: Role(sess.Role)}, nil
}

func isPublic(op *huma.Operation) bool {
	v, _ := op.Metadata[metaPublic].(bool)
	return v
}

func suspendedWritable(op *huma.Operation) bool {
	v, _ := op.Metadata[metaSuspendedWritable].(bool)
	return v
}

func isMutation(ctx huma.Context) bool {
	m := ctx.Method()
	return m != http.MethodGet && m != http.MethodHead && m != http.MethodOptions
}

func bearer(ctx huma.Context) string {
	h := ctx.Header("Authorization")
	const pfx = "Bearer "
	if len(h) > len(pfx) && strings.EqualFold(h[:len(pfx)], pfx) {
		return h[len(pfx):]
	}
	return ""
}

// problem builds an RFC 7807 error a handler can return directly.
func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

// writeProblem writes an RFC 7807 body from middleware (which cannot return an error).
func writeProblem(ctx huma.Context, status int, slug, detail string) {
	ctx.SetHeader("Content-Type", "application/problem+json")
	ctx.SetStatus(status)
	_ = json.NewEncoder(ctx.BodyWriter()).Encode(problem(status, slug, detail))
}
