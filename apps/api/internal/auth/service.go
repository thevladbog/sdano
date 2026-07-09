package auth

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
)

var (
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidRefresh     = errors.New("invalid refresh token")
	ErrInvalidInvite      = errors.New("invalid invite code")
	ErrTenantArchived     = errors.New("tenant archived")
)

// dummyPasswordHash is a valid argon2id hash that Login verifies the supplied
// password against on every credential miss (unknown email, inactive user, or
// credential-less account). Paying the ~50ms argon2id cost uniformly keeps
// response latency from revealing whether an active credentialed account exists
// at an email. It is computed once at startup with the current cost parameters,
// so its timing tracks real hashes automatically if those parameters change.
var dummyPasswordHash = mustDummyHash()

func mustDummyHash() string {
	h, err := HashPassword("dummy-password-for-constant-time-login")
	if err != nil {
		panic(fmt.Errorf("auth: precomputing dummy password hash: %w", err))
	}
	return h
}

type Service struct {
	pool   *pgxpool.Pool
	q      *db.Queries
	secret string
}

func NewService(pool *pgxpool.Pool, secret string) *Service {
	return &Service{pool: pool, q: db.New(pool), secret: secret}
}

type UserInfo struct {
	ID          uuid.UUID
	TenantID    uuid.UUID
	DisplayName string
	Email       *string
	Role        Role
}

type LoginResult struct {
	AccessToken  string
	RefreshToken string
	User         UserInfo
}

type TokenPair struct {
	AccessToken  string
	RefreshToken string
}

func (s *Service) Login(ctx context.Context, email, password string) (LoginResult, error) {
	u, err := s.q.GetUserByEmail(ctx, &email)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return LoginResult{}, fmt.Errorf("looking up user: %w", err)
	}
	// Always run one argon2id verification — against the account's real hash on
	// a hit, against a static dummy hash on every miss (unknown email, inactive
	// user, or credential-less account) — so response latency never reveals
	// whether an active credentialed account exists at this email.
	hash := dummyPasswordHash
	eligible := err == nil && u.IsActive && u.PasswordHash != nil
	if eligible {
		hash = *u.PasswordHash
	}
	ok, err := VerifyPassword(password, hash)
	if err != nil {
		return LoginResult{}, fmt.Errorf("verifying password: %w", err)
	}
	if !eligible || !ok {
		return LoginResult{}, ErrInvalidCredentials
	}
	// Credentials are valid; an archived tenant still gets no token (docs/12).
	if err := s.assertTenantNotArchived(ctx, u.TenantID); err != nil {
		return LoginResult{}, err
	}
	p := Principal{UserID: u.ID, TenantID: u.TenantID, Role: Role(u.Role)}
	access, err := IssueAccessToken(s.secret, p, AccessTTL)
	if err != nil {
		return LoginResult{}, err
	}
	refresh, err := s.issueRefresh(ctx, u.TenantID, u.ID)
	if err != nil {
		return LoginResult{}, err
	}
	return LoginResult{
		AccessToken:  access,
		RefreshToken: refresh,
		User:         UserInfo{ID: u.ID, TenantID: u.TenantID, DisplayName: u.DisplayName, Email: u.Email, Role: Role(u.Role)},
	}, nil
}

func (s *Service) Refresh(ctx context.Context, refreshPlaintext string) (TokenPair, error) {
	r, err := s.q.GetRefreshToken(ctx, HashOpaqueToken(refreshPlaintext))
	if errors.Is(err, pgx.ErrNoRows) {
		return TokenPair{}, ErrInvalidRefresh
	}
	if err != nil {
		return TokenPair{}, fmt.Errorf("looking up refresh token: %w", err)
	}
	if r.RevokedAt.Valid || !r.IsActive || r.ExpiresAt.Time.Before(time.Now()) {
		return TokenPair{}, ErrInvalidRefresh
	}
	// Do not rotate tokens for an archived tenant (docs/12).
	if err := s.assertTenantNotArchived(ctx, r.TenantID); err != nil {
		return TokenPair{}, err
	}
	if r.UsedAt.Valid {
		// Reuse of a spent token → theft. Revoke the user's whole chain.
		if err := s.q.RevokeUserRefreshTokens(ctx, r.UserID); err != nil {
			return TokenPair{}, fmt.Errorf("revoking chain: %w", err)
		}
		return TokenPair{}, ErrInvalidRefresh
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return TokenPair{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// Conditional mark-used closes the concurrent-double-use race: if another
	// refresh already spent this token between our read and now, ClaimInvite-style
	// it affects no rows -> pgx.ErrNoRows -> treat as reuse and revoke the chain.
	if _, err := qtx.MarkRefreshTokenUsed(ctx, r.ID); errors.Is(err, pgx.ErrNoRows) {
		// Roll back this tx, then revoke the chain on the pool (not the aborted tx).
		_ = tx.Rollback(ctx)
		if err := s.q.RevokeUserRefreshTokens(ctx, r.UserID); err != nil {
			return TokenPair{}, fmt.Errorf("revoking chain: %w", err)
		}
		return TokenPair{}, ErrInvalidRefresh
	} else if err != nil {
		return TokenPair{}, fmt.Errorf("marking used: %w", err)
	}

	access, err := IssueAccessToken(s.secret, Principal{UserID: r.UserID, TenantID: r.TenantID, Role: Role(r.Role)}, AccessTTL)
	if err != nil {
		return TokenPair{}, err
	}
	plain, hash, err := GenerateOpaqueToken()
	if err != nil {
		return TokenPair{}, err
	}
	if err := qtx.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		TenantID:  r.TenantID,
		UserID:    r.UserID,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(RefreshTTL), Valid: true},
	}); err != nil {
		return TokenPair{}, fmt.Errorf("inserting refresh token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return TokenPair{}, fmt.Errorf("commit: %w", err)
	}
	return TokenPair{AccessToken: access, RefreshToken: plain}, nil
}

func (s *Service) Logout(ctx context.Context, refreshPlaintext string) error {
	if err := s.q.RevokeRefreshToken(ctx, HashOpaqueToken(refreshPlaintext)); err != nil {
		return fmt.Errorf("revoking refresh token: %w", err)
	}
	return nil
}

type WorkerInfo struct {
	ID          uuid.UUID
	DisplayName string
}

type ClaimResult struct {
	DeviceToken string
	Worker      WorkerInfo
}

func (s *Service) ClaimWorker(ctx context.Context, code string) (ClaimResult, error) {
	inv, err := s.q.GetActiveInvite(ctx, code)
	if errors.Is(err, pgx.ErrNoRows) {
		return ClaimResult{}, ErrInvalidInvite
	}
	if err != nil {
		return ClaimResult{}, fmt.Errorf("looking up invite: %w", err)
	}
	// Do not mint a device token for an archived tenant (docs/12).
	if err := s.assertTenantNotArchived(ctx, inv.TenantID); err != nil {
		return ClaimResult{}, err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return ClaimResult{}, fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := s.q.WithTx(tx)

	// Conditional claim closes the single-use race: if another claim won,
	// ClaimInvite affects no rows and returns pgx.ErrNoRows.
	if _, err := qtx.ClaimInvite(ctx, inv.ID); errors.Is(err, pgx.ErrNoRows) {
		return ClaimResult{}, ErrInvalidInvite
	} else if err != nil {
		return ClaimResult{}, fmt.Errorf("claiming invite: %w", err)
	}
	plain, hash, err := GenerateOpaqueToken()
	if err != nil {
		return ClaimResult{}, err
	}
	if err := qtx.InsertDeviceToken(ctx, db.InsertDeviceTokenParams{
		TenantID: inv.TenantID, UserID: inv.UserID, TokenHash: hash,
	}); err != nil {
		return ClaimResult{}, fmt.Errorf("inserting device token: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return ClaimResult{}, fmt.Errorf("commit: %w", err)
	}
	return ClaimResult{DeviceToken: plain, Worker: WorkerInfo{ID: inv.UserID, DisplayName: inv.DisplayName}}, nil
}

// assertTenantNotArchived rejects token issuance for archived tenants: they are
// permanently dead (docs/12 — archived → 401 everywhere), so no auth endpoint
// mints tokens for them. Trial/active/suspended tenants pass; the tenant-status
// middleware handles per-request gating (e.g. suspended = read-only).
func (s *Service) assertTenantNotArchived(ctx context.Context, tenantID uuid.UUID) error {
	status, err := s.q.GetTenantStatus(ctx, tenantID)
	if err != nil {
		return fmt.Errorf("looking up tenant status: %w", err)
	}
	if status == db.TenantStatusArchived {
		return ErrTenantArchived
	}
	return nil
}

func (s *Service) issueRefresh(ctx context.Context, tenant, user uuid.UUID) (string, error) {
	plain, hash, err := GenerateOpaqueToken()
	if err != nil {
		return "", err
	}
	if err := s.q.InsertRefreshToken(ctx, db.InsertRefreshTokenParams{
		TenantID:  tenant,
		UserID:    user,
		TokenHash: hash,
		ExpiresAt: pgtype.Timestamptz{Time: time.Now().Add(RefreshTTL), Valid: true},
	}); err != nil {
		return "", fmt.Errorf("inserting refresh token: %w", err)
	}
	return plain, nil
}
