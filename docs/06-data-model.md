# Sdano — Data Model

Detailed schema for slices 0–2. PostgreSQL 16+. All timestamps are `timestamptz`. All primary keys are UUIDs; **entities created on the mobile client use client-generated UUIDs** (idempotency: re-sending an insert never creates duplicates — `ON CONFLICT (id) DO NOTHING` / `DO UPDATE`).

## Conventions

- Every domain table carries `tenant_id` (multitenancy); every sqlc query is parameterized by it. RLS may be added later without schema changes.
- Soft deletes only where history matters (`deleted_at`); hard deletes are avoided for anything referenced by reports.
- Photos and generated PDFs are **immutable** — rows are insert-only, S3 objects are never overwritten.
- Checklist templates are **versioned**: editing publishes a new version; executions pin the version they ran against, so historical reports never change retroactively.

## DDL sketch

```sql
-- === Tenancy & people =====================================================

CREATE TYPE tenant_status AS ENUM ('trial', 'active', 'suspended', 'archived');

CREATE TABLE tenant (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    name          text NOT NULL,
    status        tenant_status NOT NULL DEFAULT 'trial',
    trial_ends_at timestamptz,
    plan_note     text,          -- human-readable: "50$/mo, 20 objects, agreed 2026-08"
    billed_until  date,          -- covered-by-payment horizon
    ops_note      text,          -- operator's free-form notes
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- Operator actions audit (see 12-platform-ops.md)
CREATE TABLE ops_audit (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    action        text NOT NULL,
    tenant_id     uuid,
    detail        jsonb,
    performed_at  timestamptz NOT NULL DEFAULT now()
);

CREATE TYPE user_role AS ENUM ('admin', 'manager', 'worker');

CREATE TABLE app_user (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    role          user_role NOT NULL,
    display_name  text NOT NULL,               -- "Alexey, crew 2" is enough (PII minimalism)
    email         citext UNIQUE,                -- admins/managers only; NULL for workers
    password_hash text,                         -- argon2id; NULL for workers
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE worker_invite (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    code          text NOT NULL,                -- 6 digits, unique while active
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz
);

CREATE TABLE device_token (                     -- long-lived worker sessions
    id            uuid PRIMARY KEY,
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    token_hash    text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    revoked_at    timestamptz
);

-- === Objects ==============================================================

CREATE TABLE object (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,                -- "Lenina st., 45 — bus stop"
    address       text,
    lat           double precision,
    lon           double precision,
    kind          text,                         -- 'bus_stop' | 'entrance' | ... (free-form for now)
    qr_token      text UNIQUE,                  -- opaque token baked into the QR code
    contract_id   uuid REFERENCES contract(id), -- nullable: which contract this object belongs to
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE contract (                          -- reporting boundary ("city contract #2")
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,
    client_name   text,                          -- "City administration"
    created_at    timestamptz NOT NULL DEFAULT now()
);

-- === Checklist templates (versioned) ======================================

CREATE TABLE checklist_template (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    name          text NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE checklist_template_version (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    template_id   uuid NOT NULL REFERENCES checklist_template(id),
    version       int  NOT NULL,                -- 1, 2, 3...
    published_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (template_id, version)
);

CREATE TABLE checklist_template_item (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    version_id    uuid NOT NULL REFERENCES checklist_template_version(id),
    position      int  NOT NULL,
    title         text NOT NULL,                -- "Collect trash"
    requires_photo boolean NOT NULL DEFAULT false
);

-- === Planned loop =========================================================

CREATE TYPE work_order_status AS ENUM ('scheduled', 'in_progress', 'done', 'missed');

CREATE TABLE work_order (
    id            uuid PRIMARY KEY,              -- may be client-generated later; server-generated in slice 1
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    object_id     uuid NOT NULL REFERENCES object(id),
    version_id    uuid NOT NULL REFERENCES checklist_template_version(id), -- pinned template version
    assignee_id   uuid REFERENCES app_user(id),
    due_date      date NOT NULL,
    status        work_order_status NOT NULL DEFAULT 'scheduled',
    created_at    timestamptz NOT NULL DEFAULT now()
);
-- Recurrence (daily/weekly schedules) is deliberately deferred: slice 1 generates
-- work orders ahead of time via a simple job or by hand. A schedule table comes later.

CREATE TABLE work_execution (
    id            uuid PRIMARY KEY,              -- CLIENT-GENERATED (offline idempotency)
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    work_order_id uuid NOT NULL REFERENCES work_order(id),
    worker_id     uuid NOT NULL REFERENCES app_user(id),
    started_at    timestamptz,
    finished_at   timestamptz,                   -- set on "Sdano"
    device_finished_at timestamptz,              -- device clock at completion (offline truth)
    note          text
);

CREATE TABLE work_execution_item (
    id            uuid PRIMARY KEY,              -- CLIENT-GENERATED
    execution_id  uuid NOT NULL REFERENCES work_execution(id),
    template_item_id uuid NOT NULL REFERENCES checklist_template_item(id),
    checked       boolean NOT NULL DEFAULT false,
    checked_at    timestamptz
);

-- === Issues loop (slice 2) ================================================

CREATE TYPE issue_status AS ENUM ('open', 'assigned', 'resolved', 'verified', 'rejected');
CREATE TYPE issue_source AS ENUM ('worker', 'manager', 'public_qr');

CREATE TABLE issue (
    id            uuid PRIMARY KEY,              -- CLIENT-GENERATED when created by a worker
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    object_id     uuid NOT NULL REFERENCES object(id),
    source        issue_source NOT NULL,
    reporter_id   uuid REFERENCES app_user(id),  -- NULL for public_qr
    title         text NOT NULL,
    description   text,
    status        issue_status NOT NULL DEFAULT 'open',
    assignee_id   uuid REFERENCES app_user(id),
    due_date      date,
    created_at    timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE issue_resolution (
    id            uuid PRIMARY KEY,              -- CLIENT-GENERATED
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    issue_id      uuid NOT NULL REFERENCES issue(id),
    resolver_id   uuid NOT NULL REFERENCES app_user(id),
    execution_id  uuid REFERENCES work_execution(id), -- THE loop link: resolved within a planned visit
    resolved_at   timestamptz NOT NULL,
    note          text
);

-- === Photos (immutable) ===================================================

CREATE TYPE photo_kind AS ENUM ('before', 'after', 'defect', 'resolution');

CREATE TABLE photo (
    id            uuid PRIMARY KEY,              -- CLIENT-GENERATED; doubles as the S3 key stem
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    execution_id  uuid REFERENCES work_execution(id),
    issue_id      uuid REFERENCES issue(id),
    resolution_id uuid REFERENCES issue_resolution(id),
    kind          photo_kind NOT NULL,
    s3_key        text NOT NULL,                 -- tenants/{tenant}/photos/{id}.jpg
    taken_at      timestamptz,                   -- from EXIF / device clock
    lat           double precision,
    lon           double precision,
    uploaded_at   timestamptz,                   -- NULL = presigned URL issued, upload not yet confirmed
    CHECK (num_nonnulls(execution_id, issue_id, resolution_id) = 1)
);

-- === Reports ===============================================================

CREATE TABLE report (
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    contract_id   uuid REFERENCES contract(id),
    period_from   date NOT NULL,
    period_to     date NOT NULL,
    s3_key        text,                          -- the generated PDF
    generated_at  timestamptz,
    generated_by  uuid REFERENCES app_user(id)
);
```

## Indexing sketch

```sql
CREATE INDEX ON work_order (tenant_id, due_date, status);
CREATE INDEX ON work_order (tenant_id, object_id, due_date);
CREATE INDEX ON work_execution (tenant_id, work_order_id);
CREATE INDEX ON issue (tenant_id, status) WHERE status IN ('open','assigned');
CREATE INDEX ON issue (tenant_id, object_id);
CREATE INDEX ON photo (tenant_id, execution_id);
CREATE INDEX ON object (tenant_id) WHERE is_active;
```

## Design decisions & rationale

1. **Client-generated UUIDs on mobile-created rows** (work_execution, items, issue, photo). The offline queue can replay any mutation safely; the server upserts by primary key. This single decision removes an entire class of sync bugs.
2. **`device_finished_at` alongside `finished_at`.** Offline completions arrive late; the device clock value is the honest "when the work was done" for reports, while server receipt time remains auditable. (Device clocks can lie — both values are kept; reports use device time, disputes can reference both.)
3. **Template versioning as separate version rows** rather than copying items into executions. Executions pin `version_id`; items reference template items. Reports render historical checklists exactly as they were. Editing a template = publishing a new version; old versions are never mutated.
4. **`contract` as the reporting boundary.** The PDF is generated per contract per period — this matches how the client (the municipality) thinks. Objects link to contracts; a tenant can hold several contracts.
5. **`photo.uploaded_at` as the upload confirmation.** Flow: the client asks for a presigned URL → a photo row is created with `uploaded_at = NULL` → the client PUTs to S3 → confirms → the server verifies the object exists (HEAD) and stamps `uploaded_at`. Orphan rows with NULL are re-askable/cleanable; the queue can resume interrupted uploads.
6. **A photo belongs to exactly one parent** (execution, issue, or resolution) — enforced by the CHECK constraint. "An issue resolved within a planned visit" is expressed through `issue_resolution.execution_id`, not by double-linking photos.
7. **`missed` as a work_order status** rather than a computed value: a nightly job marks overdue scheduled orders. Reports need "missed" to be a fact with a timestamp, not a runtime opinion.
8. **Tenant lifecycle in the schema from day one.** Status (trial/active/suspended/archived) affects application behavior everywhere, so it lives in the schema even though billing itself is manual. Semantics and the evidence-is-never-hostage rule: see 12-platform-ops.md. Enforcement: one middleware, not per-handler checks.
9. **Recurrence deferred.** Slice 1 pre-generates work orders (manually or via a trivial generator). A proper schedule table (RRULE-like) is added when a real client describes their actual planning rhythm — guessing it now would bake in the wrong model.

## Open questions (to resolve with the first client)

- Does a planned job ever span multiple workers? (Current model: one execution = one worker; a second worker would create a second execution.)
- Do they need per-item photos, or per-visit before/after only? (`requires_photo` exists on items but slice 1 UI does per-visit photos.)
- Report granularity: does the municipality want one PDF per contract, per district, or per object batch?
- Photo retention: how long must evidence be kept? (Affects storage costs and the bucket lifecycle policy.)
