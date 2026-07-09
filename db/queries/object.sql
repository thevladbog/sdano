-- name: ListObjects :many
SELECT id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at
FROM object
WHERE tenant_id = $1
  AND is_active
ORDER BY name;
