package roster

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

// staffWorkerView is the wire shape for a worker. InviteCode/InviteExpiresAt are
// only ever the plaintext code — shown once to the admin who issued it, on
// create/reinvite responses, and while a worker's most recent invite is
// still pending (unclaimed and unexpired) on the list response.
type staffWorkerView struct {
	ID              uuid.UUID  `json:"id"`
	DisplayName     string     `json:"display_name"`
	IsActive        bool       `json:"is_active"`
	CreatedAt       time.Time  `json:"created_at"`
	InviteCode      *string    `json:"invite_code,omitempty"`
	InviteExpiresAt *time.Time `json:"invite_expires_at,omitempty"`
}

// Register wires the staff-facing worker roster & invite routes onto api. It
// takes the pool (not just Queries) for symmetry with sibling staff packages;
// registration never touches the pool until a request runs, so a nil pool
// (openapi generation mode) is fine.
func Register(api huma.API, pool *pgxpool.Pool) {
	q := db.New(pool)

	huma.Register(api, huma.Operation{
		OperationID: "listStaffWorkers",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/workers",
		Summary:     "List workers",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, _ *struct{}) (*listWorkersOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		rows, err := q.ListWorkers(ctx, principal.TenantID)
		if err != nil {
			return nil, fmt.Errorf("listing workers for tenant %s: %w", principal.TenantID, err)
		}
		out := &listWorkersOutput{}
		out.Body.Workers = make([]staffWorkerView, 0, len(rows))
		for _, r := range rows {
			out.Body.Workers = append(out.Body.Workers, toListedWorkerView(r))
		}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID:   "createStaffWorker",
		Method:        http.MethodPost,
		Path:          "/api/v1/staff/workers",
		Summary:       "Create a worker and issue an invite code",
		Tags:          []string{"staff"},
		DefaultStatus: http.StatusCreated,
	}, func(ctx context.Context, in *createWorkerInput) (*workerOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		row, err := q.InsertWorker(ctx, db.InsertWorkerParams{TenantID: principal.TenantID, DisplayName: in.Body.DisplayName})
		if err != nil {
			return nil, fmt.Errorf("inserting worker for tenant %s: %w", principal.TenantID, err)
		}
		code, expires, err := CreateInvite(ctx, q, principal.TenantID, row.ID)
		if err != nil {
			return nil, fmt.Errorf("creating invite for worker %s: %w", row.ID, err)
		}
		return &workerOutput{Body: staffWorkerView{
			ID: row.ID, DisplayName: row.DisplayName, IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
			InviteCode: &code, InviteExpiresAt: &expires,
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "reinviteStaffWorker",
		Method:      http.MethodPost,
		Path:        "/api/v1/staff/workers/{id}/reinvite",
		Summary:     "Issue a fresh invite code for a worker",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *reinviteWorkerInput) (*workerOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid worker id")
		}
		worker, err := q.GetWorker(ctx, db.GetWorkerParams{ID: id, TenantID: principal.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "worker-not-found", "no worker with this id")
		}
		if err != nil {
			return nil, fmt.Errorf("loading worker %s: %w", id, err)
		}
		code, expires, err := CreateInvite(ctx, q, principal.TenantID, id)
		if err != nil {
			return nil, fmt.Errorf("creating invite for worker %s: %w", id, err)
		}
		if in.Body.RevokeTokens {
			if err := q.RevokeWorkerDeviceTokens(ctx, db.RevokeWorkerDeviceTokensParams{TenantID: principal.TenantID, UserID: id}); err != nil {
				return nil, fmt.Errorf("revoking device tokens for worker %s: %w", id, err)
			}
		}
		return &workerOutput{Body: staffWorkerView{
			ID: worker.ID, DisplayName: worker.DisplayName, IsActive: worker.IsActive, CreatedAt: worker.CreatedAt.Time,
			InviteCode: &code, InviteExpiresAt: &expires,
		}}, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "patchStaffWorker",
		Method:      http.MethodPatch,
		Path:        "/api/v1/staff/workers/{id}",
		Summary:     "Rename or activate/deactivate a worker",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *patchWorkerInput) (*workerOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid worker id")
		}
		row, err := q.UpdateWorker(ctx, db.UpdateWorkerParams{
			DisplayName: in.Body.DisplayName,
			IsActive:    in.Body.IsActive,
			ID:          id,
			TenantID:    principal.TenantID,
		})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "worker-not-found", "no worker with this id")
		}
		if err != nil {
			return nil, fmt.Errorf("updating worker %s: %w", id, err)
		}
		return &workerOutput{Body: staffWorkerView{
			ID: row.ID, DisplayName: row.DisplayName, IsActive: row.IsActive, CreatedAt: row.CreatedAt.Time,
		}}, nil
	})
}

// toListedWorkerView surfaces the still-pending invite (unused, unexpired —
// see ListWorkers's DISTINCT ON) alongside the worker; a claimed or voided
// invite leaves pending_code/pending_expires_at NULL and the fields absent.
func toListedWorkerView(r db.ListWorkersRow) staffWorkerView {
	v := staffWorkerView{ID: r.ID, DisplayName: r.DisplayName, IsActive: r.IsActive, CreatedAt: r.CreatedAt.Time}
	if r.PendingCode != nil && r.PendingExpiresAt.Valid {
		v.InviteCode = r.PendingCode
		expires := r.PendingExpiresAt.Time
		v.InviteExpiresAt = &expires
	}
	return v
}

type listWorkersOutput struct {
	Body struct {
		Workers []staffWorkerView `json:"workers"`
	}
}

type createWorkerInput struct {
	Body struct {
		DisplayName string `json:"display_name" minLength:"1"`
	}
}

type reinviteWorkerInput struct {
	ID   string `path:"id"`
	Body struct {
		RevokeTokens bool `json:"revoke_tokens,omitempty"`
	}
}

type patchWorkerInput struct {
	ID   string `path:"id"`
	Body struct {
		DisplayName *string `json:"display_name,omitempty"`
		IsActive    *bool   `json:"is_active,omitempty"`
	}
}

type workerOutput struct {
	Body staffWorkerView
}
