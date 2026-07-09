package auth_test

import (
	"context"
	"errors"
	"sort"
	"sync"
	"testing"
	"time"

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

// mintRefreshToken inserts a fresh, live refresh token for the user and returns
// its plaintext — a cheap stand-in for Login that skips argon2id in hot loops.
func mintRefreshToken(t *testing.T, pool *pgxpool.Pool, tenant, user uuid.UUID) string {
	t.Helper()
	plain, hash, err := auth.GenerateOpaqueToken()
	if err != nil {
		t.Fatalf("mint token: %v", err)
	}
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO refresh_token (tenant_id, user_id, token_hash, expires_at)
		 VALUES ($1, $2, $3, now() + interval '30 days')`, tenant, user, hash); err != nil {
		t.Fatalf("insert refresh token: %v", err)
	}
	return plain
}

// countUnrevokedRefreshTokens returns how many of the user's refresh tokens are
// still live (revoked_at IS NULL) — the survivors of a chain revocation.
func countUnrevokedRefreshTokens(t *testing.T, pool *pgxpool.Pool, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(context.Background(),
		`SELECT count(*) FROM refresh_token WHERE user_id = $1 AND revoked_at IS NULL`,
		user).Scan(&n); err != nil {
		t.Fatalf("count unrevoked: %v", err)
	}
	return n
}

// TestConcurrentRefreshOfSameTokenElectsOneWinner fires N concurrent refreshes
// of the same token. Exactly one may rotate; every other must be rejected as a
// reuse (never a double-spend). Because concurrent double-use is indistinguishable
// from theft, the chain must end fully revoked.
func TestConcurrentRefreshOfSameTokenElectsOneWinner(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, user := seedStaff(t, pool, "solo@acme.test", "hunter2hunter2")
	svc := auth.NewService(pool, testSecret)

	const n = 8
	r1 := mintRefreshToken(t, pool, tenant, user)

	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = svc.Refresh(ctx, r1)
		}(i)
	}
	close(start)
	wg.Wait()

	winners := 0
	for i, err := range errs {
		switch {
		case err == nil:
			winners++
		case errors.Is(err, auth.ErrInvalidRefresh):
			// expected loser
		default:
			t.Errorf("call %d: unexpected error class: %v", i, err)
		}
	}
	if winners != 1 {
		t.Fatalf("exactly one concurrent refresh must win, got %d", winners)
	}
	if got := countUnrevokedRefreshTokens(t, pool, user); got != 0 {
		t.Fatalf("concurrent double-use must revoke the whole chain, %d token(s) survived", got)
	}
}

// TestConcurrentReuseAndRotationRevokesEntireChain races a legitimate rotation
// against attackers replaying a stolen, already-spent token. The reuse trips
// theft detection, which must revoke the ENTIRE chain — including the token the
// rotation inserts concurrently. If chain revocation's snapshot misses that
// not-yet-committed row, the thief keeps a live chain. This is the residual race.
func TestConcurrentReuseAndRotationRevokesEntireChain(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, user := seedStaff(t, pool, "race@acme.test", "hunter2hunter2")
	svc := auth.NewService(pool, testSecret)

	const iterations = 100
	const reusers = 4
	for i := 0; i < iterations; i++ {
		// Fresh chain: R1 (live) -> R2 (live); R1 is now spent.
		r1 := mintRefreshToken(t, pool, tenant, user)
		pair, err := svc.Refresh(ctx, r1)
		if err != nil {
			t.Fatalf("iter %d seed rotate: %v", i, err)
		}
		r2 := pair.RefreshToken

		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(1 + reusers)
		go func() { defer wg.Done(); <-start; _, _ = svc.Refresh(ctx, r2) }() // legit rotation R2->R3
		for j := 0; j < reusers; j++ {
			go func() { defer wg.Done(); <-start; _, _ = svc.Refresh(ctx, r1) }() // reuse of spent R1
		}
		close(start)
		wg.Wait()

		if got := countUnrevokedRefreshTokens(t, pool, user); got != 0 {
			t.Fatalf("iter %d: reuse of a spent token must revoke the whole chain, but %d token(s) survived", i, got)
		}
		if _, err := pool.Exec(ctx, `DELETE FROM refresh_token WHERE user_id = $1`, user); err != nil {
			t.Fatalf("iter %d cleanup: %v", i, err)
		}
	}
}

// waitForLockWaiters blocks until at least want backends in this database are
// parked on a heavyweight/row lock, giving concurrency tests a deterministic
// join point instead of a sleep.
func waitForLockWaiters(t *testing.T, pool *pgxpool.Pool, want int) {
	t.Helper()
	ctx := context.Background()
	deadline := time.Now().Add(10 * time.Second)
	for {
		var n int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM pg_stat_activity
			 WHERE datname = current_database() AND wait_event_type = 'Lock'`).Scan(&n); err != nil {
			t.Fatalf("poll lock waiters: %v", err)
		}
		if n >= want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d lock waiter(s), saw %d", want, n)
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// TestReuseRevocationDoesNotMissConcurrentRotation deterministically pins the
// residual race: a chain revocation (triggered by reuse of a stolen, spent
// token) must not miss a refresh row that a concurrent rotation is inserting.
//
// It parks both operations on a test-held app_user lock so the revocation takes
// its MVCC snapshot *before* the rotation's new token commits — the exact
// window in which a READ COMMITTED snapshot would skip that row. Serializing
// rotation and revocation on the per-user lock is what forces the revocation to
// see the rotated-in token.
func TestReuseRevocationDoesNotMissConcurrentRotation(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, user := seedStaff(t, pool, "det@acme.test", "hunter2hunter2")
	svc := auth.NewService(pool, testSecret)

	// Chain: R1 (spent) -> R2 (live).
	r1 := mintRefreshToken(t, pool, tenant, user)
	pair, err := svc.Refresh(ctx, r1)
	if err != nil {
		t.Fatalf("seed rotate: %v", err)
	}
	r2 := pair.RefreshToken

	// Hold app_user(user) exclusively. The rotation parks here — either at its
	// InsertRefreshToken (FK KEY SHARE vs our FOR UPDATE) or, once fixed, at its
	// own per-user LockUser — with R2 not yet rotated to a committed R3.
	holdTx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("hold begin: %v", err)
	}
	defer func() { _ = holdTx.Rollback(ctx) }()
	if _, err := holdTx.Exec(ctx, `SELECT id FROM app_user WHERE id = $1 FOR UPDATE`, user); err != nil {
		t.Fatalf("hold lock: %v", err)
	}

	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); _, _ = svc.Refresh(ctx, r2) }() // legit rotation R2 -> R3
	waitForLockWaiters(t, pool, 1)

	// Attacker replays the spent R1: its chain revocation snapshots the chain
	// now, before R3 can commit, then parks.
	go func() { defer wg.Done(); _, _ = svc.Refresh(ctx, r1) }() // reuse of spent R1
	waitForLockWaiters(t, pool, 2)

	// Let both proceed. The rotation commits R3; the revocation must still cover it.
	if err := holdTx.Commit(ctx); err != nil {
		t.Fatalf("hold commit: %v", err)
	}
	wg.Wait()

	if got := countUnrevokedRefreshTokens(t, pool, user); got != 0 {
		t.Fatalf("reuse revocation missed a concurrently rotated-in token: %d survived", got)
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

// A deactivated worker's still-valid invite must be rejected up front: the
// resulting device token could never authenticate (GetDeviceSession filters
// is_active), so claiming must fail with invite-code-invalid, not a confusing
// success. GetActiveInvite carries the is_active gate.
func TestClaimWorkerRejectsDeactivatedWorker(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	worker := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (id, tenant_id, role, display_name, is_active)
		 VALUES ($1, $2, 'worker', 'Deactivated', false)`, worker, tenant); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
		 VALUES ($1, $2, '222333', now() + interval '1 hour')`, tenant, worker); err != nil {
		t.Fatalf("invite: %v", err)
	}
	svc := auth.NewService(pool, testSecret)
	if _, err := svc.ClaimWorker(ctx, "222333"); !errors.Is(err, auth.ErrInvalidInvite) {
		t.Errorf("deactivated worker's invite must be ErrInvalidInvite, got %v", err)
	}
}

// A login's response latency must not reveal whether an active credentialed
// account exists at an email: every path — hit or miss — pays exactly one
// argon2id verification. We measure the miss paths (unknown email, inactive
// user) against a real account's wrong-password path (which runs argon2id once)
// and require each to spend at least half as long; a short-circuit spends ~0.
func TestLoginMissPathsRunPasswordVerification(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, _ := seedStaff(t, pool, "active@acme.test", "correctpassword1")

	// A deactivated account with a real password hash — the inactive miss path.
	inactiveHash, err := auth.HashPassword("inactivepassword1")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (tenant_id, role, display_name, email, password_hash, is_active)
		 VALUES ($1, 'admin', 'Gone', 'inactive@acme.test', $2, false)`, tenant, inactiveHash); err != nil {
		t.Fatalf("inactive user: %v", err)
	}
	svc := auth.NewService(pool, testSecret)

	median := func(email, password string) time.Duration {
		const n = 5
		ds := make([]time.Duration, n)
		for i := range ds {
			start := time.Now()
			_, _ = svc.Login(ctx, email, password)
			ds[i] = time.Since(start)
		}
		sort.Slice(ds, func(i, j int) bool { return ds[i] < ds[j] })
		return ds[n/2]
	}

	// Warm up: the first argon2id run allocates its 64 MiB buffer and primes any
	// one-time dummy-hash computation — keep that out of the medians.
	_, _ = svc.Login(ctx, "warmup@acme.test", "whatever")

	baseline := median("active@acme.test", "wrongpassword") // hit, wrong password → argon2id once
	unknown := median("nobody@acme.test", "whatever")        // unknown email → must still run argon2id
	inactive := median("inactive@acme.test", "inactivepassword1")

	if unknown < baseline/2 {
		t.Errorf("unknown-email login (%v) short-circuits vs baseline (%v): timing side-channel", unknown, baseline)
	}
	if inactive < baseline/2 {
		t.Errorf("inactive-user login (%v) short-circuits vs baseline (%v): timing side-channel", inactive, baseline)
	}
}

// Archived tenants are dead — the token-minting auth endpoints must not hand out
// tokens for them (docs/12: archived → 401 everywhere). Suspended tenants keep
// read-only access and must still authenticate, so only archived is blocked.

func TestLoginRejectsArchivedTenant(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, _ := seedStaff(t, pool, "boss@archived.test", "goodpassword12")
	if _, err := pool.Exec(ctx, `UPDATE tenant SET status = 'archived' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("archive: %v", err)
	}
	svc := auth.NewService(pool, testSecret)
	if _, err := svc.Login(ctx, "boss@archived.test", "goodpassword12"); !errors.Is(err, auth.ErrTenantArchived) {
		t.Errorf("login for archived tenant must be ErrTenantArchived, got %v", err)
	}
}

func TestLoginAllowsSuspendedTenant(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, _ := seedStaff(t, pool, "boss@suspended.test", "goodpassword12")
	if _, err := pool.Exec(ctx, `UPDATE tenant SET status = 'suspended' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	svc := auth.NewService(pool, testSecret)
	if _, err := svc.Login(ctx, "boss@suspended.test", "goodpassword12"); err != nil {
		t.Errorf("login for suspended tenant must succeed (read-only access), got %v", err)
	}
}

func TestRefreshRejectsArchivedTenant(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant, _ := seedStaff(t, pool, "boss@arch-refresh.test", "goodpassword12")
	svc := auth.NewService(pool, testSecret)
	res, err := svc.Login(ctx, "boss@arch-refresh.test", "goodpassword12")
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if _, err := pool.Exec(ctx, `UPDATE tenant SET status = 'archived' WHERE id = $1`, tenant); err != nil {
		t.Fatalf("archive: %v", err)
	}
	if _, err := svc.Refresh(ctx, res.RefreshToken); !errors.Is(err, auth.ErrTenantArchived) {
		t.Errorf("refresh for archived tenant must be ErrTenantArchived, got %v", err)
	}
}

func TestClaimWorkerRejectsArchivedTenant(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	worker := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name, status) VALUES ($1, 'Acme', 'archived')`, tenant); err != nil {
		t.Fatalf("tenant: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1, $2, 'worker', 'Alexey')`,
		worker, tenant); err != nil {
		t.Fatalf("worker: %v", err)
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
		 VALUES ($1, $2, '909090', now() + interval '1 hour')`, tenant, worker); err != nil {
		t.Fatalf("invite: %v", err)
	}
	svc := auth.NewService(pool, testSecret)
	if _, err := svc.ClaimWorker(ctx, "909090"); !errors.Is(err, auth.ErrTenantArchived) {
		t.Errorf("claim for archived tenant must be ErrTenantArchived, got %v", err)
	}
}
