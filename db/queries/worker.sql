-- === today bootstrap =======================================================

-- name: ListWorkerTodayOrders :many
SELECT wo.id, wo.object_id, wo.due_date, wo.status, wo.version_id,
       o.name AS object_name, o.address, o.lat, o.lon, o.qr_token
FROM work_order wo
JOIN object o ON o.id = wo.object_id
WHERE wo.tenant_id = $1 AND wo.assignee_id = $2 AND wo.due_date = $3
ORDER BY o.name;

-- name: ListChecklistItemsByVersions :many
SELECT version_id, id, position, title, requires_photo
FROM checklist_template_item
WHERE version_id = ANY(sqlc.arg(version_ids)::uuid[])
ORDER BY version_id, position;

-- === execution upsert ======================================================

-- name: GetWorkOrderForWorker :one
SELECT id, object_id, assignee_id, status, version_id
FROM work_order
WHERE id = $1 AND tenant_id = $2;

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
    device_finished_at = EXCLUDED.device_finished_at,
    -- finished_at = server time of the *most recent* completion (last-write-wins
    -- domain semantics), not a first-completion-only stamp:
    --   * reopen: a later snapshot with device_finished_at NULL resets
    --     finished_at to NULL (the CASE's NULL branch).
    --   * (re)complete: COALESCE stamps a fresh now() unless finished_at is
    --     already set for *this* device_finished_at value, so replaying the
    --     same completed snapshot is idempotent (stable finished_at).
    --   * out-of-order delivery (a stale in-progress snapshot arriving after
    --     a completed one) is bounded by the mobile outbox, which coalesces
    --     to the latest snapshot per entity before sync — see docs/08. This
    --     query does not itself guard against out-of-order replay.
    finished_at        = CASE
        WHEN EXCLUDED.device_finished_at IS NULL THEN NULL
        ELSE COALESCE(work_execution.finished_at, now())
    END,
    note = EXCLUDED.note
WHERE work_execution.tenant_id = EXCLUDED.tenant_id
  AND work_execution.worker_id = EXCLUDED.worker_id;

-- name: DeleteExecutionItemsNotIn :exec
DELETE FROM work_execution_item
WHERE execution_id = $1 AND id <> ALL(sqlc.arg(keep_ids)::uuid[]);

-- name: UpsertWorkExecutionItem :exec
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
-- uploaded_at stamped once (COALESCE); taken_at/lat/lon are device values,
-- deterministic across replays.
UPDATE photo
SET uploaded_at = COALESCE(uploaded_at, now()),
    taken_at    = $3,
    lat         = $4,
    lon         = $5
WHERE id = $1 AND tenant_id = $2;

-- === QR resolution =========================================================

-- name: GetObjectByQr :one
SELECT id, name, address, lat, lon, kind, qr_token, is_active
FROM object
WHERE tenant_id = $1 AND qr_token = $2;

-- name: GetWorkerOrderForObject :one
SELECT id, object_id, due_date, status, version_id
FROM work_order
WHERE tenant_id = $1 AND assignee_id = $2 AND object_id = $3 AND due_date = $4
ORDER BY created_at DESC
LIMIT 1;
