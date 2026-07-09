package auth_test

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/testdb"
)

const testSecret = "test-secret"

// seedStaff inserts a tenant + an active admin with the given password.
func seedStaff(t *testing.T, pool *pgxpool.Pool, email, password string) (tenant, user uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, user = uuid.New(), uuid.New()
	hash, err := auth.HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("insert tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (id, tenant_id, role, display_name, email, password_hash)
		 VALUES ($1, $2, 'admin', 'Boss', $3, $4)`, user, tenant, email, hash); err != nil {
		t.Fatalf("insert user: %v", err)
	}
	return tenant, user
}

func TestLoginRefreshRotationAndReuseRevokesChain(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	seedStaff(t, pool, "boss@acme.test", "hunter2hunter2")
	svc := auth.NewService(pool, testSecret)

	res, err := svc.Login(ctx, "boss@acme.test", "hunter2hunter2")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if res.AccessToken == "" || res.RefreshToken == "" {
		t.Fatal("login must return both tokens")
	}
	if _, err := auth.ParseAccessToken(testSecret, res.AccessToken); err != nil {
		t.Errorf("access token must be valid: %v", err)
	}

	// Rotation: R1 -> R2.
	pair, err := svc.Refresh(ctx, res.RefreshToken)
	if err != nil {
		t.Fatalf("refresh R1: %v", err)
	}
	if pair.RefreshToken == res.RefreshToken {
		t.Error("refresh must rotate to a new refresh token")
	}

	// Reuse of the spent R1 must fail AND revoke the whole chain (R2 too).
	if _, err := svc.Refresh(ctx, res.RefreshToken); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Errorf("reusing R1 must be ErrInvalidRefresh, got %v", err)
	}
	if _, err := svc.Refresh(ctx, pair.RefreshToken); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Errorf("after reuse-detection, R2 must be revoked, got %v", err)
	}
}

func TestLoginRejectsBadPasswordAndInactive(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, user := seedStaff(t, pool, "a@b.test", "rightpassword1")
	svc := auth.NewService(pool, testSecret)

	if _, err := svc.Login(ctx, "a@b.test", "wrongpassword"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("wrong password must be ErrInvalidCredentials, got %v", err)
	}
	if _, err := svc.Login(ctx, "missing@b.test", "whatever"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("unknown email must be ErrInvalidCredentials, got %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE app_user SET is_active = false WHERE id = $1`, user); err != nil {
		t.Fatalf("deactivate: %v", err)
	}
	_ = tenant
	if _, err := svc.Login(ctx, "a@b.test", "rightpassword1"); !errors.Is(err, auth.ErrInvalidCredentials) {
		t.Errorf("inactive user must be ErrInvalidCredentials, got %v", err)
	}
}

func TestLogoutRevokesRefreshToken(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	seedStaff(t, pool, "c@d.test", "password12345")
	svc := auth.NewService(pool, testSecret)

	res, _ := svc.Login(ctx, "c@d.test", "password12345")
	if err := svc.Logout(ctx, res.RefreshToken); err != nil {
		t.Fatalf("logout: %v", err)
	}
	if _, err := svc.Refresh(ctx, res.RefreshToken); !errors.Is(err, auth.ErrInvalidRefresh) {
		t.Errorf("refresh after logout must fail, got %v", err)
	}
}

func TestClaimWorkerIsSingleUse(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	worker := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1, $2, 'worker', 'Alexey')`,
		worker, tenant); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
		 VALUES ($1, $2, '123456', now() + interval '1 hour')`, tenant, worker); err != nil {
		t.Fatalf("invite: %v", err)
	}
	svc := auth.NewService(pool, testSecret)

	res, err := svc.ClaimWorker(ctx, "123456")
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if res.DeviceToken == "" || res.Worker.DisplayName != "Alexey" {
		t.Fatalf("bad claim result: %+v", res)
	}
	// The device token authenticates as the worker.
	got, err := db.New(pool).GetDeviceSession(ctx, auth.HashOpaqueToken(res.DeviceToken))
	if err != nil || got.UserID != worker {
		t.Errorf("device token must authenticate as the worker: got=%+v err=%v", got, err)
	}

	// Second claim of the same code fails (single use).
	if _, err := svc.ClaimWorker(ctx, "123456"); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("second claim must be ErrInvalidInvite, got %v", err)
	}
}

func TestClaimWorkerRejectsExpiredInvite(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	worker := uuid.New()
	_, _ = pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant)
	_, _ = pool.Exec(ctx, `INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1, $2, 'worker', 'Old')`, worker, tenant)
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
		 VALUES ($1, $2, '654321', now() - interval '1 minute')`, tenant, worker); err != nil {
		t.Fatalf("invite: %v", err)
	}
	svc := auth.NewService(pool, testSecret)
	if _, err := svc.ClaimWorker(ctx, "654321"); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("expired invite must be ErrInvalidInvite, got %v", err)
	}
}
