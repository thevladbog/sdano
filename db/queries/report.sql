-- name: InsertReport :one
INSERT INTO report (tenant_id, contract_id, period_from, period_to, generated_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, status, created_at;

-- name: GetReport :one
SELECT id, contract_id, period_from, period_to, status, failure_reason, s3_key, generated_at, created_at
FROM report WHERE id = $1 AND tenant_id = $2;

-- name: ListReports :many
SELECT id, contract_id, period_from, period_to, status, generated_at, created_at
FROM report WHERE tenant_id = $1
ORDER BY created_at DESC LIMIT 100;

-- name: ClaimNextReport :one
-- Worker-internal: drains the queue across ALL tenants (single in-process
-- worker). SKIP LOCKED lets a future second instance coexist safely.
UPDATE report SET render_attempts = render_attempts + 1
WHERE id = (
    SELECT id FROM report
    WHERE status = 'generating' AND render_attempts < 3
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, tenant_id, contract_id, period_from, period_to, render_attempts;

-- name: MarkReportReady :exec
UPDATE report SET status = 'ready', s3_key = $2, generated_at = now()
WHERE id = $1 AND status = 'generating';

-- name: MarkReportFailed :exec
UPDATE report SET status = 'failed', failure_reason = $2
WHERE id = $1 AND status = 'generating';

-- name: GetTenantName :one
SELECT name FROM tenant WHERE id = $1;

-- name: GetContractName :one
SELECT name, client_name FROM contract WHERE id = $1 AND tenant_id = $2;

-- name: ReportSummaryRows :many
SELECT o.id AS object_id, o.name AS object_name, o.address,
       count(wo.id) AS planned,
       count(wo.id) FILTER (WHERE wo.status = 'done') AS done,
       count(wo.id) FILTER (WHERE wo.status = 'missed') AS missed
FROM work_order wo
JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
WHERE wo.tenant_id = sqlc.arg(tenant_id)
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND (sqlc.narg(contract_id)::uuid IS NULL OR o.contract_id = sqlc.narg(contract_id))
GROUP BY o.id, o.name, o.address
ORDER BY o.address NULLS LAST, o.name;

-- name: ReportObjectExecutions :many
SELECT wo.object_id, e.id AS execution_id, wo.due_date,
       e.device_finished_at, e.finished_at, u.display_name AS worker_name,
       (SELECT count(*) FROM work_execution_item i WHERE i.execution_id = e.id AND i.checked) AS checked_items,
       (SELECT count(*) FROM work_execution_item i WHERE i.execution_id = e.id) AS total_items
FROM work_execution e
JOIN work_order wo ON wo.id = e.work_order_id AND wo.tenant_id = e.tenant_id
JOIN app_user u ON u.id = e.worker_id
WHERE e.tenant_id = sqlc.arg(tenant_id)
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND e.device_finished_at IS NOT NULL
  AND (sqlc.narg(contract_id)::uuid IS NULL
       OR EXISTS (SELECT 1 FROM object o WHERE o.id = wo.object_id AND o.contract_id = sqlc.narg(contract_id)))
ORDER BY wo.object_id, e.device_finished_at;

-- name: ReportExecutionPhotos :many
SELECT execution_id, id, kind, s3_key, taken_at, lat, lon, uploaded_at
FROM photo
WHERE tenant_id = sqlc.arg(tenant_id)
  AND execution_id = ANY(sqlc.arg(execution_ids)::uuid[])
ORDER BY execution_id, kind, id;

-- name: ReportMissedOrders :many
SELECT wo.object_id, o.name AS object_name, wo.due_date
FROM work_order wo
JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
WHERE wo.tenant_id = sqlc.arg(tenant_id)
  AND wo.status = 'missed'
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND (sqlc.narg(contract_id)::uuid IS NULL OR o.contract_id = sqlc.narg(contract_id))
ORDER BY o.address NULLS LAST, o.name, wo.due_date;
