-- Tenant-local "today" (phase-5 decision, docs/07): IANA zone name, set by the
-- operator (ops CLI, phase 6). Default UTC preserves prior behavior.
ALTER TABLE tenant ADD COLUMN timezone text NOT NULL DEFAULT 'UTC';
-- rules §5: created_at on everything; work_execution lacked it. Server receipt
-- time of the first upsert — the keyset for staff executions-history pagination.
ALTER TABLE work_execution ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX work_execution_tenant_created_idx ON work_execution (tenant_id, created_at DESC, id DESC);
