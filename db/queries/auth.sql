-- name: GetUserByEmail :one
SELECT id, tenant_id, role, display_name, email, password_hash, is_active
FROM app_user
WHERE email = $1;

-- name: GetTenantStatus :one
SELECT status FROM tenant WHERE id = $1;

-- name: InsertRefreshToken :exec
INSERT INTO refresh_token (tenant_id, user_id, token_hash, expires_at)
VALUES ($1, $2, $3, $4);

-- name: GetRefreshToken :one
SELECT r.id, r.tenant_id, r.user_id, r.expires_at, r.used_at, r.revoked_at,
       u.role, u.is_active
FROM refresh_token r
JOIN app_user u ON u.id = r.user_id
WHERE r.token_hash = $1;

-- name: MarkRefreshTokenUsed :exec
UPDATE refresh_token SET used_at = now() WHERE id = $1;

-- name: RevokeUserRefreshTokens :exec
UPDATE refresh_token SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL;

-- name: RevokeRefreshToken :exec
UPDATE refresh_token SET revoked_at = now()
WHERE token_hash = $1 AND revoked_at IS NULL;

-- name: InsertDeviceToken :exec
INSERT INTO device_token (tenant_id, user_id, token_hash)
VALUES ($1, $2, $3);

-- name: GetDeviceSession :one
SELECT u.id AS user_id, u.tenant_id, u.role
FROM device_token t
JOIN app_user u ON u.id = t.user_id
WHERE t.token_hash = $1
  AND t.revoked_at IS NULL
  AND u.is_active;

-- name: GetActiveInvite :one
SELECT i.id, i.tenant_id, i.user_id, u.display_name
FROM worker_invite i
JOIN app_user u ON u.id = i.user_id
WHERE i.code = $1 AND i.used_at IS NULL AND i.expires_at > now();

-- name: ClaimInvite :one
UPDATE worker_invite SET used_at = now()
WHERE id = $1 AND used_at IS NULL
RETURNING id;
