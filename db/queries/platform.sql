-- name: ListTenantTimezones :many
-- Scheduler-internal: the per-tenant fan-out for missed-order marking. Every
-- status is included -- a suspended tenant's order history must stay honest
-- too (docs/12: suspension is read-only, not history-freezing).
SELECT id, timezone FROM tenant;

-- name: MarkTenantOverdueOrdersMissed :execrows
-- One tenant's slice of the hourly missed-marking sweep: an order is missed
-- once the tenant's local date has moved past due_date. tenant_today is
-- computed in Go (Scheduler.markOverdueMissed) so one tenant's invalid
-- timezone can never fail the sweep for every other tenant.
UPDATE work_order SET status = 'missed'
WHERE tenant_id = sqlc.arg(tenant_id)
  AND status = 'scheduled'
  AND due_date < sqlc.arg(tenant_today);

-- name: ListOrphanPhotos :many
SELECT id, s3_key FROM photo
WHERE uploaded_at IS NULL AND created_at < $1
ORDER BY created_at LIMIT 100;

-- name: DeletePhotoRow :exec
DELETE FROM photo WHERE id = $1 AND uploaded_at IS NULL;

-- name: OpsCreateTenant :one
INSERT INTO tenant (name, trial_ends_at) VALUES ($1, $2)
RETURNING id, name, status, trial_ends_at;

-- name: OpsListTenants :many
SELECT t.id, t.name, t.status, t.timezone, t.trial_ends_at, t.billed_until, t.suspended_at,
       (SELECT count(*) FROM app_user u WHERE u.tenant_id = t.id AND u.role = 'worker' AND u.is_active) AS active_workers,
       (SELECT count(*) FROM object o WHERE o.tenant_id = t.id AND o.is_active) AS active_objects
FROM tenant t ORDER BY t.name;

-- name: OpsSetTenantStatus :exec
UPDATE tenant SET status = $2, suspended_at = $3 WHERE id = $1;

-- name: OpsSetBilling :exec
UPDATE tenant SET billed_until = $2, plan_note = COALESCE(sqlc.narg(plan_note), plan_note)
WHERE id = $1;

-- name: InsertOpsAudit :exec
INSERT INTO ops_audit (action, tenant_id, detail) VALUES ($1, $2, $3);

-- name: OpsInsertAdminUser :one
-- The operator CLI's only user-creation path: OpsCreateTenant's first admin.
-- Role is hardcoded 'admin' -- ops never creates any other role.
INSERT INTO app_user (tenant_id, role, display_name, email, password_hash)
VALUES ($1, 'admin', $2, $3, $4)
RETURNING id;

-- name: GetTenantSuspension :one
SELECT suspended_at FROM tenant WHERE id = $1;

-- name: GetTenantByName :one
-- The demo seeder's idempotence guard (cmd/seed): tenant.name has no unique
-- constraint (multiple real customers could share a display name), so this
-- is a plain lookup, not a uniqueness check — the seeder only ever queries
-- its own fixed demo tenant name.
SELECT id FROM tenant WHERE name = $1;

-- name: SetTenantTimezone :execrows
-- Guarded by pg_timezone_names: an unknown zone updates zero rows (the
-- caller treats 0 as an error), so an invalid string can never enter
-- tenant.timezone through this path and later trip the scheduler's or the
-- report renderer's zone lookup.
UPDATE tenant SET timezone = $2
WHERE id = $1 AND EXISTS (SELECT 1 FROM pg_catalog.pg_timezone_names WHERE name = $2);
