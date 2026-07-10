-- name: MarkOverdueOrdersMissed :execrows
-- Tenant-timezone-aware: an order is missed once ITS tenant's local date has
-- moved past due_date. Cross-tenant by design (the scheduler serves all tenants).
UPDATE work_order wo SET status = 'missed'
FROM tenant t
WHERE t.id = wo.tenant_id
  AND wo.status = 'scheduled'
  AND wo.due_date < (now() AT TIME ZONE t.timezone)::date;

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
