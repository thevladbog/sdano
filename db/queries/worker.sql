-- === today bootstrap =======================================================

-- name: ListWorkerTodayOrders :many
SELECT wo.id, wo.object_id, wo.due_date, wo.status, wo.version_id,
       o.name AS object_name, o.address, o.lat, o.lon, o.qr_token
FROM work_order wo
JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
WHERE wo.tenant_id = $1 AND wo.assignee_id = $2 AND wo.due_date = $3
ORDER BY o.name;

-- name: ListChecklistItemsByVersions :many
SELECT i.version_id, i.id, i.position, i.title, i.requires_photo
FROM checklist_template_item i
JOIN checklist_template_version v ON v.id = i.version_id
JOIN checklist_template t ON t.id = v.template_id
WHERE i.version_id = ANY(sqlc.arg(version_ids)::uuid[])
  AND t.tenant_id = sqlc.arg(tenant_id)
ORDER BY i.version_id, i.position;

-- === execution upsert ======================================================

-- name: GetWorkOrderForWorker :one
-- Locked: called inside UpsertExecution's transaction so the assignment
-- check and the rest of the upsert share one row lock on work_order. This
-- serializes a concurrent staff reassignment (UpdateWorkOrder) against an
-- in-flight worker upsert instead of letting both interleave and leave the
-- execution bound to a worker who is no longer assigned.
-- FOR UPDATE (not FOR SHARE): the same transaction later UPDATEs this row
-- (SetWorkOrderStatus); a shared lock would deadlock two concurrent upserts
-- of the same order on lock upgrade. Exclusive from the start serializes them.
SELECT id, object_id, assignee_id, status, version_id
FROM work_order
WHERE id = $1 AND tenant_id = $2
FOR UPDATE;

-- name: UpsertWorkExecution :exec
-- Full-state idempotent upsert. finished_at is server receipt time, stamped
-- once when device_finished_at first becomes non-null (COALESCE keeps it stable
-- across replays). The ON CONFLICT WHERE guard prevents a colliding id from a
-- different tenant/worker overwriting an existing row (defense in depth).
INSERT INTO work_execution (
    id, tenant_id, work_order_id, worker_id, started_at, device_finished_at, finished_at, note
) VALUES (
    sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(work_order_id), sqlc.arg(worker_id),
    sqlc.arg(started_at), sqlc.arg(device_finished_at),
    CASE WHEN sqlc.arg(device_finished_at)::timestamptz IS NOT NULL THEN now() ELSE NULL END,
    sqlc.arg(note)
)
ON CONFLICT (id) DO UPDATE SET
    started_at         = EXCLUDED.started_at,
    -- Completion is evidence: once device_finished_at / finished_at are recorded
    -- they are permanent. A later in-progress snapshot (a delayed/out-of-order
    -- outbox retry) can never erase a recorded completion time. started_at / note
    -- still follow the latest snapshot.
    device_finished_at = COALESCE(work_execution.device_finished_at, EXCLUDED.device_finished_at),
    finished_at        = COALESCE(work_execution.finished_at,
                                  CASE WHEN EXCLUDED.device_finished_at IS NOT NULL THEN now() ELSE NULL END),
    note = EXCLUDED.note
WHERE work_execution.tenant_id = EXCLUDED.tenant_id
  AND work_execution.worker_id = EXCLUDED.worker_id;

-- name: DeleteExecutionItemsNotIn :exec
DELETE FROM work_execution_item
WHERE execution_id = $1 AND id <> ALL(sqlc.arg(keep_ids)::uuid[]);

-- name: UpsertWorkExecutionItem :execrows
INSERT INTO work_execution_item (id, execution_id, template_item_id, checked, checked_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
    checked    = EXCLUDED.checked,
    checked_at = EXCLUDED.checked_at
WHERE work_execution_item.execution_id = EXCLUDED.execution_id;

-- name: SetWorkOrderStatus :exec
UPDATE work_order SET status = $3 WHERE id = $1 AND tenant_id = $2;

-- === execution read (server view) ==========================================

-- name: GetExecution :one
SELECT id, work_order_id, worker_id, started_at, finished_at, device_finished_at, note
FROM work_execution
WHERE id = $1 AND tenant_id = $2;

-- name: GetExecutionForWorker :one
SELECT id, tenant_id, worker_id
FROM work_execution
WHERE id = $1 AND tenant_id = $2;

-- name: ListExecutionItems :many
SELECT id, template_item_id, checked, checked_at
FROM work_execution_item
WHERE execution_id = $1
ORDER BY id;

-- name: ListExecutionPhotos :many
SELECT id, kind, taken_at, lat, lon, uploaded_at
FROM photo
WHERE execution_id = $1
ORDER BY id;

-- === photos (two-phase) ====================================================

-- name: InsertPhotoPresign :exec
INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING;

-- name: GetPhoto :one
SELECT id, tenant_id, execution_id, kind, s3_key, taken_at, lat, lon, uploaded_at
FROM photo
WHERE id = $1 AND tenant_id = $2;

-- name: ConfirmPhoto :exec
-- uploaded_at, taken_at, lat, lon are all stamped once (COALESCE): evidence
-- is immutable, so a re-confirm replay cannot overwrite already-set values.
UPDATE photo
SET uploaded_at = COALESCE(uploaded_at, now()),
    taken_at    = COALESCE(taken_at, $3),
    lat         = COALESCE(lat, $4),
    lon         = COALESCE(lon, $5)
WHERE id = $1 AND tenant_id = $2;

-- === QR resolution =========================================================

-- name: GetObjectByQr :one
SELECT id, name, address, lat, lon, kind, qr_token, is_active
FROM object
WHERE tenant_id = $1 AND qr_token = $2 AND is_active;

-- name: GetWorkerOrderForObject :one
SELECT id, object_id, due_date, status, version_id
FROM work_order
WHERE tenant_id = $1 AND assignee_id = $2 AND object_id = $3 AND due_date = $4
ORDER BY created_at DESC
LIMIT 1;

-- === tenant settings ========================================================

-- name: GetTenantTimezone :one
SELECT timezone FROM tenant WHERE id = $1;
