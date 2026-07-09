package auth

import (
	"context"
	"errors"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

// RegisterAuthRoutes registers the public /api/v1/auth/* endpoints.
func RegisterAuthRoutes(api huma.API, svc *Service) {
	huma.Register(api, huma.Operation{
		OperationID: "authLogin", Method: http.MethodPost, Path: "/api/v1/auth/login",
		Summary: "Staff login", Tags: []string{"auth"}, Metadata: Public(),
	}, func(ctx context.Context, in *loginInput) (*loginOutput, error) {
		res, err := svc.Login(ctx, in.Body.Email, in.Body.Password)
		if errors.Is(err, ErrInvalidCredentials) {
			return nil, problem(http.StatusUnauthorized, "invalid-credentials", "email or password is incorrect")
		}
		if errors.Is(err, ErrTenantArchived) {
			return nil, problem(http.StatusUnauthorized, "tenant-archived", "tenant has been archived")
		}
		if err != nil {
			return nil, err
		}
		out := &loginOutput{}
		out.Body.AccessToken = res.AccessToken
		out.Body.RefreshToken = res.RefreshToken
		out.Body.User = userView{ID: res.User.ID, DisplayName: res.User.DisplayName, Email: res.User.Email, Role: string(res.User.Role)}
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "authRefresh", Method: http.MethodPost, Path: "/api/v1/auth/refresh",
		Summary: "Rotate tokens", Tags: []string{"auth"}, Metadata: Public(),
	}, func(ctx context.Context, in *refreshInput) (*tokenPairOutput, error) {
		pair, err := svc.Refresh(ctx, in.Body.RefreshToken)
		if errors.Is(err, ErrInvalidRefresh) {
			return nil, problem(http.StatusUnauthorized, "invalid-refresh-token", "refresh token is invalid or expired")
		}
		if errors.Is(err, ErrTenantArchived) {
			return nil, problem(http.StatusUnauthorized, "tenant-archived", "tenant has been archived")
		}
		if err != nil {
			return nil, err
		}
		out := &tokenPairOutput{}
		out.Body.AccessToken = pair.AccessToken
		out.Body.RefreshToken = pair.RefreshToken
		return out, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "authLogout", Method: http.MethodPost, Path: "/api/v1/auth/logout",
		Summary: "Revoke a refresh token", Tags: []string{"auth"}, Metadata: Public(),
		DefaultStatus: http.StatusNoContent,
	}, func(ctx context.Context, in *logoutInput) (*struct{}, error) {
		if err := svc.Logout(ctx, in.Body.RefreshToken); err != nil {
			return nil, err
		}
		return nil, nil
	})

	huma.Register(api, huma.Operation{
		OperationID: "authWorkerClaim", Method: http.MethodPost, Path: "/api/v1/auth/worker/claim",
		Summary: "Claim a worker invite", Tags: []string{"auth"}, Metadata: Public(),
	}, func(ctx context.Context, in *claimInput) (*claimOutput, error) {
		res, err := svc.ClaimWorker(ctx, in.Body.InviteCode)
		if errors.Is(err, ErrInvalidInvite) {
			return nil, problem(http.StatusUnauthorized, "invite-code-invalid", "invite code is invalid or already used")
		}
		if errors.Is(err, ErrTenantArchived) {
			return nil, problem(http.StatusUnauthorized, "tenant-archived", "tenant has been archived")
		}
		if err != nil {
			return nil, err
		}
		out := &claimOutput{}
		out.Body.DeviceToken = res.DeviceToken
		out.Body.Worker = workerView{ID: res.Worker.ID, DisplayName: res.Worker.DisplayName}
		return out, nil
	})
}

type loginInput struct {
	Body struct {
		Email    string `json:"email" format:"email"`
		Password string `json:"password" minLength:"1"`
	}
}

type userView struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
	Email       *string   `json:"email"`
	Role        string    `json:"role"`
}

type loginOutput struct {
	Body struct {
		AccessToken  string   `json:"access_token"`
		RefreshToken string   `json:"refresh_token"`
		User         userView `json:"user"`
	}
}

type refreshInput struct {
	Body struct {
		RefreshToken string `json:"refresh_token"`
	}
}

type tokenPairOutput struct {
	Body struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
}

type logoutInput struct {
	Body struct {
		RefreshToken string `json:"refresh_token"`
	}
}

type claimInput struct {
	Body struct {
		InviteCode string `json:"invite_code" minLength:"1"`
		// DeviceName is accepted for forward-compat but not stored (no column yet).
		DeviceName *string `json:"device_name,omitempty"`
	}
}

type workerView struct {
	ID          uuid.UUID `json:"id"`
	DisplayName string    `json:"display_name"`
}

type claimOutput struct {
	Body struct {
		DeviceToken string     `json:"device_token"`
		Worker      workerView `json:"worker"`
	}
}
