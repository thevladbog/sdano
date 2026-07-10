DROP INDEX IF EXISTS work_execution_tenant_created_idx;
ALTER TABLE work_execution DROP COLUMN IF EXISTS created_at;
ALTER TABLE tenant DROP COLUMN IF EXISTS timezone;
