-- name: ListObjects :many
SELECT id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at
FROM object
WHERE tenant_id = $1
  AND (sqlc.narg(active)::boolean IS NULL OR is_active = sqlc.narg(active))
  AND (sqlc.narg(contract_id)::uuid IS NULL OR contract_id = sqlc.narg(contract_id))
ORDER BY name;
