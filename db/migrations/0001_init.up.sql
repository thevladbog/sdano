CREATE EXTENSION IF NOT EXISTS citext;

-- === Enums =================================================================

CREATE TYPE tenant_status     AS ENUM ('trial', 'active', 'suspended', 'archived');
CREATE TYPE user_role         AS ENUM ('admin', 'manager', 'worker');
CREATE TYPE work_order_status AS ENUM ('scheduled', 'in_progress', 'done', 'missed');
CREATE TYPE issue_status      AS ENUM ('open', 'assigned', 'resolved', 'verified', 'rejected');
CREATE TYPE issue_source      AS ENUM ('worker', 'manager', 'public_qr');
CREATE TYPE photo_kind        AS ENUM ('before', 'after', 'defect', 'resolution');
CREATE TYPE report_status     AS ENUM ('generating', 'ready', 'failed');

-- === Tenancy & people ======================================================

CREATE TABLE tenant (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    status        tenant_status NOT NULL DEFAULT 'trial',
    trial_ends_at timestamptz,
    plan_note     text,
    billed_until  date,
    ops_note      text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE ops_audit (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action        text NOT NULL,
    tenant_id     uuid,
    detail        jsonb,
    performed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE app_user (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    role          user_role NOT NULL,
    display_name  text NOT NULL,
    email         citext UNIQUE,
    password_hash text,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE worker_invite (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    code          text NOT NULL,
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);

-- 6-digit codes are unique while unclaimed (docs/06: "unique while active").
CREATE UNIQUE INDEX worker_invite_active_code_idx ON worker_invite (code) WHERE used_at IS NULL;

CREATE TABLE device_token (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    token_hash    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);

-- Spec addition (2026-07-09 design): staff refresh tokens, rotated, hashed at rest.
CREATE TABLE refresh_token (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    token_hash    text NOT NULL UNIQUE,
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,
    revoked_at    timestamptz
);

CREATE INDEX refresh_token_user_idx ON refresh_token (user_id) WHERE revoked_at IS NULL;

-- === Objects ===============================================================

CREATE TABLE contract (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,
    client_name   text,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE object (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,
    address       text,
    lat           double precision,
    lon           double precision,
    kind          text,
    qr_token      text UNIQUE,
    contract_id   uuid REFERENCES contract(id),
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- === Checklist templates (versioned) =======================================

CREATE TABLE checklist_template (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE checklist_template_version (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id   uuid NOT NULL REFERENCES checklist_template(id),
    version       int  NOT NULL,
    published_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

CREATE TABLE checklist_template_item (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version_id     uuid NOT NULL REFERENCES checklist_template_version(id),
    position       int  NOT NULL,
    title          text NOT NULL,
    requires_photo boolean NOT NULL DEFAULT false
);

-- === Planned loop ==========================================================

CREATE TABLE work_order (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    object_id     uuid NOT NULL REFERENCES object(id),
    version_id    uuid NOT NULL REFERENCES checklist_template_version(id),
    assignee_id   uuid REFERENCES app_user(id),
    due_date      date NOT NULL,
    status        work_order_status NOT NULL DEFAULT 'scheduled',
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Client-generated UUIDs (offline idempotency): deliberately NO defaults below.

CREATE TABLE work_execution (
    id                 uuid PRIMARY KEY,
    tenant_id          uuid NOT NULL REFERENCES tenant(id),
    work_order_id      uuid NOT NULL REFERENCES work_order(id),
    worker_id          uuid NOT NULL REFERENCES app_user(id),
    started_at         timestamptz,
    finished_at        timestamptz,
    device_finished_at timestamptz,
    note               text
);

CREATE TABLE work_execution_item (
    id               uuid PRIMARY KEY,
    execution_id     uuid NOT NULL REFERENCES work_execution(id),
    template_item_id uuid NOT NULL REFERENCES checklist_template_item(id),
    checked          boolean NOT NULL DEFAULT false,
    checked_at       timestamptz
);

-- === Issues loop (slice 2 — schema now, API later) =========================

CREATE TABLE issue (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    object_id     uuid NOT NULL REFERENCES object(id),
    source        issue_source NOT NULL,
    reporter_id   uuid REFERENCES app_user(id),
    title         text NOT NULL,
    description   text,
    status        issue_status NOT NULL DEFAULT 'open',
    assignee_id   uuid REFERENCES app_user(id),
    due_date      date,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE issue_resolution (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    issue_id      uuid NOT NULL REFERENCES issue(id),
    resolver_id   uuid NOT NULL REFERENCES app_user(id),
    execution_id  uuid REFERENCES work_execution(id),
    resolved_at   timestamptz NOT NULL,
    note          text
);

-- === Photos (immutable) ====================================================

CREATE TABLE photo (
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    execution_id  uuid REFERENCES work_execution(id),
    issue_id      uuid REFERENCES issue(id),
    resolution_id uuid REFERENCES issue_resolution(id),
    kind          photo_kind NOT NULL,
    s3_key        text NOT NULL,
    taken_at      timestamptz,
    lat           double precision,
    lon           double precision,
    uploaded_at   timestamptz,
    CHECK (num_nonnulls(execution_id, issue_id, resolution_id) = 1)
);

-- === Reports ===============================================================

CREATE TABLE report (
    id             uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id      uuid NOT NULL REFERENCES tenant(id),
    contract_id    uuid REFERENCES contract(id),
    period_from    date NOT NULL,
    period_to      date NOT NULL,
    -- Spec addition (2026-07-09 design): report rows double as the render queue.
    status         report_status NOT NULL DEFAULT 'generating',
    failure_reason text,
    s3_key         text,
    generated_at   timestamptz,
    generated_by   uuid REFERENCES app_user(id)
);

-- === Indexes (docs/06) =====================================================

CREATE INDEX work_order_tenant_due_status_idx ON work_order (tenant_id, due_date, status);
CREATE INDEX work_order_tenant_object_due_idx ON work_order (tenant_id, object_id, due_date);
CREATE INDEX work_execution_tenant_order_idx  ON work_execution (tenant_id, work_order_id);
CREATE INDEX issue_tenant_open_idx            ON issue (tenant_id, status) WHERE status IN ('open', 'assigned');
CREATE INDEX issue_tenant_object_idx          ON issue (tenant_id, object_id);
CREATE INDEX photo_tenant_execution_idx       ON photo (tenant_id, execution_id);
CREATE INDEX object_tenant_active_idx         ON object (tenant_id) WHERE is_active;
