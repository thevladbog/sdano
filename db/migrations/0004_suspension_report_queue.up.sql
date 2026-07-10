-- Operator suspension timestamp: the precise evidence carve-out boundary
-- (docs/12 — work with device_finished_at < suspended_at is always accepted).
ALTER TABLE tenant ADD COLUMN suspended_at timestamptz;
-- rules §5: created_at on everything; report rows double as the render queue.
ALTER TABLE report ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE report ADD COLUMN render_attempts int NOT NULL DEFAULT 0;
ALTER TABLE photo ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX report_queue_idx ON report (created_at) WHERE status = 'generating';
