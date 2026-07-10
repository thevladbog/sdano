-- === contracts ==============================================================

-- name: InsertContract :one
INSERT INTO contract (tenant_id, name, client_name)
VALUES ($1, $2, $3)
RETURNING id, name, client_name, created_at;

-- === objects ===============================================================

-- name: InsertObject :one
INSERT INTO object (tenant_id, name, address, lat, lon, kind, qr_token, contract_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at;

-- name: UpdateObject :one
UPDATE object SET
    name        = COALESCE(sqlc.narg(name), name),
    address     = COALESCE(sqlc.narg(address), address),
    lat         = COALESCE(sqlc.narg(lat), lat),
    lon         = COALESCE(sqlc.narg(lon), lon),
    kind        = COALESCE(sqlc.narg(kind), kind),
    qr_token    = COALESCE(sqlc.narg(qr_token), qr_token),
    contract_id = COALESCE(sqlc.narg(contract_id), contract_id),
    is_active   = COALESCE(sqlc.narg(is_active), is_active)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at;

-- name: GetObject :one
SELECT id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at
FROM object WHERE id = $1 AND tenant_id = $2;

-- name: ListObjectExecutions :many
SELECT e.id, e.work_order_id, e.created_at, e.started_at, e.finished_at, e.device_finished_at,
       u.display_name AS worker_name,
       (SELECT count(*) FROM photo p WHERE p.execution_id = e.id AND p.uploaded_at IS NOT NULL) AS photo_count
FROM work_execution e
JOIN work_order wo ON wo.id = e.work_order_id AND wo.tenant_id = e.tenant_id
JOIN app_user u ON u.id = e.worker_id
WHERE e.tenant_id = sqlc.arg(tenant_id) AND wo.object_id = sqlc.arg(object_id)
  AND (e.created_at, e.id) < (sqlc.arg(after_created_at)::timestamptz, sqlc.arg(after_id)::uuid)
ORDER BY e.created_at DESC, e.id DESC
LIMIT sqlc.arg(page_limit);

-- === checklist templates ====================================================

-- name: InsertChecklistTemplate :one
INSERT INTO checklist_template (tenant_id, name)
VALUES ($1, $2)
RETURNING id, name, created_at;

-- name: InsertChecklistTemplateVersion :one
INSERT INTO checklist_template_version (template_id, version)
VALUES ($1, $2)
RETURNING id, template_id, version, published_at;

-- name: InsertChecklistTemplateItem :one
INSERT INTO checklist_template_item (version_id, position, title, requires_photo)
VALUES ($1, $2, $3, $4)
RETURNING id, version_id, position, title, requires_photo;

-- === work orders ===========================================================

-- name: InsertWorkOrder :exec
INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: UpdateWorkOrder :one
UPDATE work_order SET
    assignee_id = COALESCE(sqlc.narg(assignee_id), assignee_id),
    due_date    = COALESCE(sqlc.narg(due_date), due_date)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id)
RETURNING id, object_id, version_id, assignee_id, due_date, status, created_at;

-- name: GetWorkOrder :one
SELECT id, object_id, version_id, assignee_id, due_date, status, created_at
FROM work_order WHERE id = $1 AND tenant_id = $2;

-- name: ListWorkOrders :many
SELECT id, object_id, version_id, assignee_id, due_date, status, created_at
FROM work_order
WHERE tenant_id = sqlc.arg(tenant_id)
  AND (sqlc.narg(due_date)::date IS NULL OR due_date = sqlc.narg(due_date))
  AND (sqlc.narg(object_id)::uuid IS NULL OR object_id = sqlc.narg(object_id))
  AND (sqlc.narg(assignee_id)::uuid IS NULL OR assignee_id = sqlc.narg(assignee_id))
  AND (sqlc.narg(status)::work_order_status IS NULL OR status = sqlc.narg(status))
ORDER BY due_date DESC, created_at DESC
LIMIT 500;

-- name: CountObjectsInTenant :one
SELECT count(*) FROM object WHERE tenant_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: CountVersionsInTenant :one
SELECT count(*) FROM checklist_template_version v
JOIN checklist_template t ON t.id = v.template_id
WHERE t.tenant_id = $1 AND v.id = ANY(sqlc.arg(ids)::uuid[]);

-- name: CountActiveWorkersInTenant :one
SELECT count(*) FROM app_user
WHERE tenant_id = $1 AND role = 'worker' AND is_active AND id = ANY(sqlc.arg(ids)::uuid[]);

-- name: CountContractsInTenant :one
SELECT count(*) FROM contract WHERE tenant_id = $1 AND id = ANY(sqlc.arg(ids)::uuid[]);

-- === workers & invites =====================================================

-- name: ListWorkers :many
-- sqlc (v1.31.1) does not propagate LEFT JOIN nullability through a derived
-- table/LATERAL: pending_code/pending_expires_at would be typed as NOT NULL
-- (Go string, not *string) even though most workers have no pending invite,
-- and pgx panics scanning SQL NULL into a non-nullable string. Joining
-- worker_invite directly (a real table) lets sqlc infer nullability
-- correctly; DISTINCT ON collapses to the single latest unused invite per
-- worker, and the outer SELECT re-sorts by display_name since DISTINCT ON's
-- ORDER BY must lead with the distinct key (u.id).
SELECT * FROM (
    SELECT DISTINCT ON (u.id) u.id, u.display_name, u.is_active, u.created_at,
           wi.code AS pending_code, wi.expires_at AS pending_expires_at
    FROM app_user u
    LEFT JOIN worker_invite wi ON wi.user_id = u.id AND wi.used_at IS NULL AND wi.expires_at > now()
    WHERE u.tenant_id = sqlc.arg(tenant_id) AND u.role = 'worker'
    ORDER BY u.id, wi.expires_at DESC NULLS LAST
) sub
ORDER BY display_name;

-- name: InsertWorker :one
INSERT INTO app_user (tenant_id, role, display_name)
VALUES ($1, 'worker', $2)
RETURNING id, display_name, is_active, created_at;

-- name: GetWorker :one
SELECT id, display_name, is_active, created_at
FROM app_user WHERE id = $1 AND tenant_id = $2 AND role = 'worker';

-- name: UpdateWorker :one
UPDATE app_user SET
    display_name = COALESCE(sqlc.narg(display_name), display_name),
    is_active    = COALESCE(sqlc.narg(is_active), is_active)
WHERE id = sqlc.arg(id) AND tenant_id = sqlc.arg(tenant_id) AND role = 'worker'
RETURNING id, display_name, is_active, created_at;

-- name: InsertInvite :exec
INSERT INTO worker_invite (tenant_id, user_id, code, expires_at)
VALUES ($1, $2, $3, $4);

-- name: VoidWorkerInvites :exec
UPDATE worker_invite SET used_at = now()
WHERE tenant_id = $1 AND user_id = $2 AND used_at IS NULL;

-- name: RevokeWorkerDeviceTokens :exec
UPDATE device_token SET revoked_at = now()
WHERE tenant_id = $1 AND user_id = $2 AND revoked_at IS NULL;

-- === evidence reads ========================================================

-- name: GetExecutionDetail :one
SELECT e.id, e.work_order_id, e.created_at, e.started_at, e.finished_at, e.device_finished_at, e.note,
       u.display_name AS worker_name,
       wo.object_id, o.name AS object_name
FROM work_execution e
JOIN app_user u ON u.id = e.worker_id
JOIN work_order wo ON wo.id = e.work_order_id AND wo.tenant_id = e.tenant_id
JOIN object o ON o.id = wo.object_id AND o.tenant_id = e.tenant_id
WHERE e.id = $1 AND e.tenant_id = $2;

-- name: ListExecutionItemsWithTitles :many
SELECT ei.id, ei.template_item_id, ei.checked, ei.checked_at, ti.position, ti.title
FROM work_execution_item ei
JOIN work_execution e ON e.id = ei.execution_id AND e.tenant_id = sqlc.arg(tenant_id)
JOIN checklist_template_item ti ON ti.id = ei.template_item_id
WHERE ei.execution_id = sqlc.arg(execution_id)
ORDER BY ti.position;

-- name: GetPhotoForStaff :one
SELECT id, execution_id, kind, s3_key, taken_at, lat, lon, uploaded_at
FROM photo WHERE id = $1 AND tenant_id = $2;

-- === dashboard =============================================================

-- name: ListDashboardOrders :many
-- Same LEFT-JOIN-through-a-derived-table nullability gap as ListWorkers:
-- worker_name must come from a directly-joined base table (app_user) for
-- sqlc to type it *string instead of a NOT NULL string that panics on scan
-- when a work order has no execution yet. work_execution is also joined
-- directly so finished_at/created_at nullability is unambiguous. DISTINCT ON
-- picks the single latest execution per order; the photo-count correlated
-- subquery naturally yields 0 (not NULL) when e.id is NULL, so no COALESCE
-- is needed. The outer SELECT re-sorts by object_name.
SELECT * FROM (
    SELECT DISTINCT ON (wo.id)
        wo.id AS order_id, wo.status, wo.due_date,
        o.id AS object_id, o.name AS object_name, o.address, o.lat, o.lon,
        u.display_name AS worker_name, e.finished_at AS last_finished_at, e.created_at AS last_activity_at,
        (SELECT count(*) FROM photo p WHERE p.execution_id = e.id AND p.uploaded_at IS NOT NULL) AS photo_count
    FROM work_order wo
    JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
    LEFT JOIN work_execution e ON e.work_order_id = wo.id AND e.tenant_id = wo.tenant_id
    LEFT JOIN app_user u ON u.id = e.worker_id
    WHERE wo.tenant_id = sqlc.arg(tenant_id) AND wo.due_date = sqlc.arg(due_date)
    ORDER BY wo.id, e.created_at DESC NULLS LAST
) sub
ORDER BY object_name;
