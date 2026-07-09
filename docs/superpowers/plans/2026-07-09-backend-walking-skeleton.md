# Backend Walking Skeleton Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Prove the entire Sdano type pipeline end-to-end (SQL → sqlc → Go → huma → OpenAPI 3.1 → orval → TS) with the full database schema, dev docker-compose, and CI — one tracer endpoint, everything else grows on these rails.

**Architecture:** Nx monorepo with a single Go binary (`apps/api`, chi + huma), plain-SQL migrations (golang-migrate), sqlc-generated data access, orval-generated TS client in `packages/api-client`. Spec: `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md`. This plan covers spec phases 1–2 only; auth, worker API, staff API, and reports are separate follow-up plans.

**Tech Stack:** Go 1.26.5, PostgreSQL 18.4, huma v2 (≥2.37) + chi v5, sqlc 1.31.1, golang-migrate v4, pgx v5, aws-sdk-go-v2, Nx (latest, 23.x), orval 8 (≥8.0.3), Node 24 LTS, golangci-lint 2.12.2, testcontainers-go, MinIO (dev), GitHub Actions.

## Global Constraints

- **Versions are policy:** every dependency is adopted at its latest stable, security-clean version. Where this plan writes `@latest` / `npm i -E <pkg>@latest`, resolve at execution time, verify against context7 docs if the API is unfamiliar, and let `go.sum` / `package-lock.json` pin the result. Known-good floors as of 2026-07-09: Go 1.26.5, postgres 18.4, sqlc 1.31.1, golangci-lint 2.12.2, huma ≥v2.37, **orval ≥8.0.3 (earlier 8.x has RCE CVE-2026-24132)**.
- **Never hand-edit generated code**: `apps/api/internal/db/` (sqlc), `packages/api-client/src/generated/` (orval), `packages/api-client/openapi.json`. Regenerate and commit.
- **`tenant_id` on every domain query**, taken from the authenticated principal, never from URL or body.
- **Evidence is sacred**: photos/executions/reports are insert-only; no code path may silently drop them.
- **Conventional commits** (`feat:`, `fix:`, `docs:`, `chore:`), small and single-purpose. Every commit leaves CI-relevant checks green (`make lint test`).
- **Docs and code never diverge**: a change that contradicts a doc updates the doc in the same commit series.
- Code, comments, commits: **English**. Secrets only via env; every new env var goes into `.env.example` in the same commit.
- Zero golangci-lint warnings; no `fmt.Println` (structured `slog` only); errors wrapped with context at package boundaries.
- Go module path is `sdano.app/api`. All Go commands run from `apps/api/` unless stated otherwise.

---

### Task 1: Agent docs & root monorepo layout

**Files:**
- Create: `CLAUDE.md`
- Modify: `11-development-rules.md` (§7 Security & data rules — extend the dependencies bullet)
- Modify: `AGENTS.md` (Workflow expectations section)
- Create: `apps/admin/README.md`, `apps/mobile/README.md` (placeholders)
- Modify: `.gitignore`

**Interfaces:**
- Consumes: nothing (first task).
- Produces: the root directory layout (`apps/`, later tasks add `db/`, `deploy/`, `packages/`) and the version-policy text that all later tasks obey.

- [ ] **Step 1: Create `CLAUDE.md`**

```markdown
# CLAUDE.md

Read `AGENTS.md` fully before making any change — it is the operational digest of this repo's rules. `docs/` is normative; `11-development-rules.md` is the full rulebook with rationale.

Non-negotiables (details in AGENTS.md):

- The type pipeline is law: SQL → sqlc → Go → huma → OpenAPI → orval → TS. Never hand-edit generated code — regenerate it.
- `tenant_id` from the authenticated principal on every domain query.
- Evidence (photos, executions, reports) is immutable and never silently dropped.
- Dependencies: latest stable, security-clean versions; verify current version and API via context7 before adding or upgrading; `govulncheck` and `npm audit` must stay green.
- Conventional commits; docs updated in the same PR as behavior changes.
```

- [ ] **Step 2: Extend the dependencies bullet in `11-development-rules.md` §7**

Find this line in §7:

```markdown
- Dependencies: renovate/dependabot on; a new dependency needs a sentence of justification in the PR ("no stdlib/existing-dep way to do X reasonably").
```

Append directly after it:

```markdown
- **Version policy:** every dependency is adopted at its latest stable, security-clean version at the time of adoption. Before adding or upgrading, verify the current version and API against live documentation (context7 for AI agents); pin exact versions (`go.mod`/`go.sum`, `package-lock.json`). `govulncheck` (Go) and `npm audit` (TS) run in CI and block merge on known vulnerabilities. Upgrades land as dedicated PRs, never bundled into feature work.
```

- [ ] **Step 3: Add the digest line to `AGENTS.md`**

In the "Workflow expectations" section, after the `**Generated code:** ...` bullet, insert:

```markdown
- **Dependencies:** adopt the latest stable, security-clean version; check the current version and API via context7 before adding or upgrading; pin exactly (`go.sum`, `package-lock.json`). `govulncheck` and `npm audit` gate CI.
```

- [ ] **Step 4: Create frontend placeholder directories**

`apps/admin/README.md`:

```markdown
# Sdano Admin (placeholder)

Next.js admin panel — scaffolded and developed in the frontend phase.
See `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md` (Repository structure) and `docs/03-roadmap.md`.
```

`apps/mobile/README.md`:

```markdown
# Sdano Mobile (placeholder)

Expo/React Native worker app — scaffolded and developed in the frontend phase.
See `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md` (Repository structure) and `docs/03-roadmap.md`.
```

- [ ] **Step 5: Extend `.gitignore`**

Replace the "Dependencies & build output" section of `.gitignore` with:

```gitignore
# Dependencies & build output
node_modules/
dist/
.nx/
*.log
apps/api/bin/
coverage/
```

- [ ] **Step 6: Verify and commit**

Run: `ls apps/admin apps/mobile && cat CLAUDE.md`
Expected: both READMEs listed, CLAUDE.md contents printed.

```bash
git add CLAUDE.md AGENTS.md 11-development-rules.md apps/ .gitignore
git commit -m "docs: agent rules — dependency version policy, CLAUDE.md, frontend placeholders"
```

---

### Task 2: Nx workspace

**Files:**
- Create: `package.json`, `nx.json`, `.nvmrc`
- Create: `apps/api/project.json`, `packages/api-client/project.json`

**Interfaces:**
- Consumes: root layout from Task 1.
- Produces: Nx targets `nx build api`, `nx test api`, `nx lint api`, `nx generate api-client` that later tasks and CI call. Root `package.json` with npm workspaces `["apps/*", "packages/*"]`.

- [ ] **Step 1: Create root `package.json` and `.nvmrc`**

`package.json`:

```json
{
  "name": "sdano",
  "version": "0.0.0",
  "private": true,
  "workspaces": ["apps/*", "packages/*"],
  "engines": { "node": ">=24" },
  "scripts": {
    "build": "nx run-many -t build",
    "test": "nx run-many -t test",
    "lint": "nx run-many -t lint"
  }
}
```

`.nvmrc`:

```
24
```

- [ ] **Step 2: Install Nx (latest, exact-pinned)**

Run from repo root: `npm install -D -E nx@latest`
Expected: `package-lock.json` created; `node_modules/` present (gitignored). Nx major should be 23.x (latest as of 2026-07); if npm reports a newer major, take it — version policy.

- [ ] **Step 3: Create `nx.json`**

```json
{
  "$schema": "./node_modules/nx/schemas/nx-schema.json",
  "namedInputs": {
    "default": ["{projectRoot}/**/*"]
  },
  "targetDefaults": {}
}
```

- [ ] **Step 4: Create Nx project files**

`apps/api/project.json`:

```json
{
  "name": "api",
  "$schema": "../../node_modules/nx/schemas/project-schema.json",
  "projectType": "application",
  "targets": {
    "build": {
      "executor": "nx:run-commands",
      "options": { "command": "go build ./...", "cwd": "apps/api" }
    },
    "test": {
      "executor": "nx:run-commands",
      "options": { "command": "go test ./...", "cwd": "apps/api" }
    },
    "lint": {
      "executor": "nx:run-commands",
      "options": { "command": "golangci-lint run", "cwd": "apps/api" }
    }
  }
}
```

`packages/api-client/project.json`:

```json
{
  "name": "api-client",
  "$schema": "../../node_modules/nx/schemas/project-schema.json",
  "projectType": "library",
  "targets": {
    "generate": {
      "executor": "nx:run-commands",
      "options": { "command": "npm run generate", "cwd": "packages/api-client" }
    }
  }
}
```

- [ ] **Step 5: Verify Nx sees both projects**

Run: `npx nx show projects`
Expected output (order may vary):

```
api
api-client
```

(`nx build api` will fail until Task 5 creates Go sources — that is expected; do not run it yet.)

- [ ] **Step 6: Commit**

```bash
git add package.json package-lock.json nx.json .nvmrc apps/api/project.json packages/api-client/project.json
git commit -m "chore: nx workspace with api and api-client projects"
```

---

### Task 3: Dev infrastructure (docker-compose) + Makefile + .env

**Files:**
- Create: `deploy/docker-compose.yml`
- Create: `.env.example`
- Create: `Makefile`

**Interfaces:**
- Consumes: nothing.
- Produces: `make dev-up` / `make dev-down`; a running `postgres:18.4` on `localhost:5432` and MinIO on `localhost:9000` with bucket `sdano-evidence`; compose network `sdano_default` (used by `make migrate-up` in Task 4). Env var names fixed here are consumed by `config.Load` in Task 5: `HTTP_ADDR`, `DATABASE_URL`, `S3_ENDPOINT`, `S3_REGION`, `S3_BUCKET`, `S3_ACCESS_KEY`, `S3_SECRET_KEY`, `S3_USE_PATH_STYLE`, `ADMIN_ORIGIN`, `DEV_TENANT_HEADER_AUTH`.

- [ ] **Step 1: Create `.env.example`**

```bash
# --- HTTP ---
HTTP_ADDR=:8080

# --- Database ---
POSTGRES_USER=sdano
POSTGRES_PASSWORD=sdano-dev-password
POSTGRES_DB=sdano
DATABASE_URL=postgres://sdano:sdano-dev-password@localhost:5432/sdano?sslmode=disable

# --- Object storage (MinIO in dev; managed S3-compatible in prod) ---
S3_ENDPOINT=http://localhost:9000
S3_REGION=us-east-1
S3_BUCKET=sdano-evidence
S3_ACCESS_KEY=sdano-dev
S3_SECRET_KEY=sdano-dev-secret
S3_USE_PATH_STYLE=true

# --- Admin panel origin (CORS) ---
ADMIN_ORIGIN=http://localhost:3000

# --- Dev-only flags ---
# Enables header-based tenant auth (X-Dev-Tenant-Id) for the walking-skeleton
# tracer endpoint. Replaced by real auth in the auth plan. NEVER set in production.
DEV_TENANT_HEADER_AUTH=true
```

Then run: `cp .env.example .env`

- [ ] **Step 2: Create `deploy/docker-compose.yml`**

```yaml
name: sdano

services:
  postgres:
    image: postgres:18.4
    profiles: [dev]
    environment:
      POSTGRES_USER: ${POSTGRES_USER}
      POSTGRES_PASSWORD: ${POSTGRES_PASSWORD}
      POSTGRES_DB: ${POSTGRES_DB}
    ports:
      - "5432:5432"
    volumes:
      - pg_data:/var/lib/postgresql/data
    healthcheck:
      test: ["CMD-SHELL", "pg_isready -U ${POSTGRES_USER} -d ${POSTGRES_DB}"]
      interval: 5s
      timeout: 3s
      retries: 10

  minio:
    image: minio/minio:latest
    profiles: [dev]
    command: server /data --console-address ":9001"
    environment:
      MINIO_ROOT_USER: ${S3_ACCESS_KEY}
      MINIO_ROOT_PASSWORD: ${S3_SECRET_KEY}
    ports:
      - "9000:9000"
      - "9001:9001"
    volumes:
      - minio_data:/data
    healthcheck:
      test: ["CMD-SHELL", "curl -sf http://localhost:9000/minio/health/live || exit 1"]
      interval: 5s
      timeout: 3s
      retries: 10

  minio-setup:
    image: minio/mc:latest
    profiles: [dev]
    depends_on:
      minio:
        condition: service_healthy
    environment:
      S3_ACCESS_KEY: ${S3_ACCESS_KEY}
      S3_SECRET_KEY: ${S3_SECRET_KEY}
      S3_BUCKET: ${S3_BUCKET}
    entrypoint: >
      /bin/sh -c "
      mc alias set local http://minio:9000 $$S3_ACCESS_KEY $$S3_SECRET_KEY &&
      mc mb --ignore-existing local/$$S3_BUCKET &&
      echo 'bucket ready'
      "

  # Pinned Chrome for the PDF report renderer (reports plan). Present from day
  # one so the compose topology is stable; pin an exact tag at first real use.
  headless-shell:
    image: chromedp/headless-shell:latest
    profiles: [dev]
    ports:
      - "9222:9222"

volumes:
  pg_data:
  minio_data:
```

Note: if the `minio` healthcheck fails because the image no longer ships `curl`, replace the test with `["CMD", "mc", "ready", "local"]` — check `docker compose logs minio` before changing anything else.

- [ ] **Step 3: Create `Makefile`**

```makefile
COMPOSE := docker compose -f deploy/docker-compose.yml --env-file .env
MIGRATIONS := $(CURDIR)/db/migrations
# Runs on the compose network so `postgres` resolves on macOS and Linux alike.
MIGRATE := docker run --rm -v $(MIGRATIONS):/migrations --network sdano_default \
  migrate/migrate:v4 -path=/migrations \
  -database "postgres://$(POSTGRES_USER):$(POSTGRES_PASSWORD)@postgres:5432/$(POSTGRES_DB)?sslmode=disable"

# Optional: absent in CI (drift jobs don't need it), required for dev targets.
-include .env
export

.PHONY: dev-up dev-down migrate-up migrate-down migrate-drop generate-sqlc openapi generate-client generate lint test drift

dev-up:
	$(COMPOSE) --profile dev up -d --wait

dev-down:
	$(COMPOSE) --profile dev down

migrate-up:
	$(MIGRATE) up

migrate-down:
	$(MIGRATE) down 1

migrate-drop:
	$(MIGRATE) drop -f

generate-sqlc:
	sqlc generate

openapi:
	cd apps/api && go run ./cmd/api openapi > ../../packages/api-client/openapi.json

generate-client: openapi
	cd packages/api-client && npm run generate

generate: generate-sqlc generate-client

lint:
	cd apps/api && golangci-lint run

test:
	cd apps/api && go test ./...

drift: generate
	git diff --exit-code
```

(`migrate-*`, `generate-*`, `lint`, `test` targets reference tools set up in Tasks 4–9 — defining them all now keeps the Makefile a single-commit artifact.)

- [ ] **Step 4: Verify infra comes up**

Run: `make dev-up`
Expected: postgres, minio, minio-setup, headless-shell all started; `--wait` returns once healthchecks pass; `docker compose -f deploy/docker-compose.yml --env-file .env logs minio-setup` shows `bucket ready`.

Run: `docker compose -f deploy/docker-compose.yml --env-file .env ps --format '{{.Service}} {{.Status}}'`
Expected: `postgres` and `minio` report `Up ... (healthy)`.

- [ ] **Step 5: Commit**

```bash
git add deploy/docker-compose.yml .env.example Makefile
git commit -m "chore: dev docker-compose (postgres, minio, headless-shell) and makefile"
```

---

### Task 4: Migration 0001 — full schema (slices 0–2) + docs/06 merge

**Files:**
- Create: `db/migrations/0001_init.up.sql`
- Create: `db/migrations/0001_init.down.sql`
- Modify: `docs/06-data-model.md` (add `refresh_token`, `report.status`/`failure_reason`)

**Interfaces:**
- Consumes: running postgres from Task 3 (`make dev-up`).
- Produces: the complete schema every later task queries. Table/column names below are the contract for all sqlc queries.

- [ ] **Step 1: Write `db/migrations/0001_init.up.sql`**

The DDL is `docs/06-data-model.md` verbatim, plus the two spec additions (`refresh_token`, `report.status`/`failure_reason`), plus `citext`, ordered for FK dependencies (contract before object):

```sql
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
```

- [ ] **Step 2: Write `db/migrations/0001_init.down.sql`**

```sql
DROP TABLE IF EXISTS report;
DROP TABLE IF EXISTS photo;
DROP TABLE IF EXISTS issue_resolution;
DROP TABLE IF EXISTS issue;
DROP TABLE IF EXISTS work_execution_item;
DROP TABLE IF EXISTS work_execution;
DROP TABLE IF EXISTS work_order;
DROP TABLE IF EXISTS checklist_template_item;
DROP TABLE IF EXISTS checklist_template_version;
DROP TABLE IF EXISTS checklist_template;
DROP TABLE IF EXISTS object;
DROP TABLE IF EXISTS contract;
DROP TABLE IF EXISTS refresh_token;
DROP TABLE IF EXISTS device_token;
DROP TABLE IF EXISTS worker_invite;
DROP TABLE IF EXISTS app_user;
DROP TABLE IF EXISTS ops_audit;
DROP TABLE IF EXISTS tenant;

DROP TYPE IF EXISTS report_status;
DROP TYPE IF EXISTS photo_kind;
DROP TYPE IF EXISTS issue_source;
DROP TYPE IF EXISTS issue_status;
DROP TYPE IF EXISTS work_order_status;
DROP TYPE IF EXISTS user_role;
DROP TYPE IF EXISTS tenant_status;

DROP EXTENSION IF EXISTS citext;
```

- [ ] **Step 3: Verify up → down → up against the dev database**

Run (infra from Task 3 must be up):

```bash
make migrate-up && make migrate-down && make migrate-up
```

Expected: three successful runs, no errors. Then verify the schema landed:

```bash
docker compose -f deploy/docker-compose.yml --env-file .env exec postgres \
  psql -U sdano -d sdano -c "\dt" | grep -c ' table '
```

Expected: `18` (17 domain tables + `schema_migrations`).

- [ ] **Step 4: Merge the two additions into `docs/06-data-model.md`**

In the DDL sketch, after the `device_token` table, insert:

```sql
CREATE TABLE refresh_token (                    -- staff sessions (rotated)
    id            uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    tenant_id     uuid NOT NULL REFERENCES tenant(id),
    user_id       uuid NOT NULL REFERENCES app_user(id),
    token_hash    text NOT NULL UNIQUE,          -- sha256; opaque 256-bit token, 30-day TTL
    created_at    timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    used_at       timestamptz,                   -- rotation: reuse of a used token revokes the chain
    revoked_at    timestamptz
);
```

In the `report` table DDL, after the `period_to` line, insert:

```sql
    status        report_status NOT NULL DEFAULT 'generating',  -- 'generating' | 'ready' | 'failed'
    failure_reason text,
```

And append to the "Design decisions & rationale" list:

```markdown
10. **Report rows double as the render queue** (`status` on `report`, one in-process renderer goroutine). Changed on 2026-07-09: added with the backend walking-skeleton design, see `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md`.
11. **Staff refresh tokens in `refresh_token`** — opaque, hashed, rotated on every use; reuse of a spent token revokes the user's whole chain. Added 2026-07-09 (same design doc).
```

- [ ] **Step 5: Commit**

```bash
git add db/migrations/ docs/06-data-model.md
git commit -m "feat: full schema migration (slices 0-2) with refresh_token and report status"
```

---

### Task 5: Go API skeleton — config, slog, huma server, static /healthz

**Files:**
- Create: `apps/api/go.mod` (module `sdano.app/api`, go 1.26)
- Create: `apps/api/internal/config/config.go`
- Test: `apps/api/internal/config/config_test.go`
- Create: `apps/api/internal/app/app.go`
- Create: `apps/api/cmd/api/main.go`
- Create: `apps/api/.golangci.yml`

**Interfaces:**
- Consumes: env var names from Task 3's `.env.example`.
- Produces (used by every later task):
  - `config.Load(getenv func(string) string) (Config, error)` — `Config` fields: `HTTPAddr, DatabaseURL, S3Endpoint, S3Region, S3Bucket, S3AccessKey, S3SecretKey string; S3UsePathStyle, DevTenantHeaderAuth bool; AdminOrigin string`.
  - `app.New(cfg config.Config, deps app.Deps) (*chi.Mux, huma.API)` — registers all routes; `app.Deps` starts as `struct{ Checks []app.HealthCheck }` and grows (`Pool`, `S3` in Task 7, `Queries` in Task 8).
  - `type app.HealthCheck struct { Name string; Ping func(context.Context) error }`

- [ ] **Step 1: Initialize the module and pull dependencies**

```bash
cd apps/api
go mod init sdano.app/api
go get github.com/danielgtaylor/huma/v2@latest
go get github.com/go-chi/chi/v5@latest
```

Expected: `go.mod` with `go 1.26` (toolchain 1.26.5) and huma ≥v2.37, chi v5.x in `go.sum`.

- [ ] **Step 2: Write the failing config test**

`apps/api/internal/config/config_test.go`:

```go
package config

import "testing"

func fakeEnv(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func TestLoadAppliesDefaults(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		"DATABASE_URL":  "postgres://u:p@localhost:5432/db",
		"S3_ENDPOINT":   "http://localhost:9000",
		"S3_BUCKET":     "sdano-evidence",
		"S3_ACCESS_KEY": "k",
		"S3_SECRET_KEY": "s",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.HTTPAddr != ":8080" {
		t.Errorf("HTTPAddr default = %q, want :8080", cfg.HTTPAddr)
	}
	if cfg.S3Region != "us-east-1" {
		t.Errorf("S3Region default = %q, want us-east-1", cfg.S3Region)
	}
	if cfg.DevTenantHeaderAuth {
		t.Error("DevTenantHeaderAuth must default to false")
	}
}

func TestLoadRequiresDatabaseURL(t *testing.T) {
	_, err := Load(fakeEnv(map[string]string{
		"S3_ENDPOINT": "e", "S3_BUCKET": "b", "S3_ACCESS_KEY": "k", "S3_SECRET_KEY": "s",
	}))
	if err == nil {
		t.Fatal("Load must fail without DATABASE_URL")
	}
}

func TestLoadParsesBools(t *testing.T) {
	cfg, err := Load(fakeEnv(map[string]string{
		"DATABASE_URL": "d", "S3_ENDPOINT": "e", "S3_BUCKET": "b",
		"S3_ACCESS_KEY": "k", "S3_SECRET_KEY": "s",
		"S3_USE_PATH_STYLE": "true", "DEV_TENANT_HEADER_AUTH": "true",
	}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !cfg.S3UsePathStyle || !cfg.DevTenantHeaderAuth {
		t.Error("boolean env vars 'true' must parse to true")
	}
}
```

- [ ] **Step 3: Run the test to verify it fails**

Run: `go test ./internal/config/`
Expected: FAIL — `undefined: Load` (compile error).

- [ ] **Step 4: Implement `config.Load`**

`apps/api/internal/config/config.go`:

```go
// Package config loads all runtime configuration from the environment
// (12-factor: no config files, every variable listed in .env.example).
package config

import (
	"errors"
	"fmt"
	"strconv"
)

type Config struct {
	HTTPAddr            string
	DatabaseURL         string
	S3Endpoint          string
	S3Region            string
	S3Bucket            string
	S3AccessKey         string
	S3SecretKey         string
	S3UsePathStyle      bool
	AdminOrigin         string
	DevTenantHeaderAuth bool
}

func Load(getenv func(string) string) (Config, error) {
	cfg := Config{
		HTTPAddr:    withDefault(getenv("HTTP_ADDR"), ":8080"),
		DatabaseURL: getenv("DATABASE_URL"),
		S3Endpoint:  getenv("S3_ENDPOINT"),
		S3Region:    withDefault(getenv("S3_REGION"), "us-east-1"),
		S3Bucket:    getenv("S3_BUCKET"),
		S3AccessKey: getenv("S3_ACCESS_KEY"),
		S3SecretKey: getenv("S3_SECRET_KEY"),
		AdminOrigin: getenv("ADMIN_ORIGIN"),
	}

	var err error
	if cfg.S3UsePathStyle, err = parseBool(getenv, "S3_USE_PATH_STYLE"); err != nil {
		return Config{}, err
	}
	if cfg.DevTenantHeaderAuth, err = parseBool(getenv, "DEV_TENANT_HEADER_AUTH"); err != nil {
		return Config{}, err
	}

	var missing []string
	for name, v := range map[string]string{
		"DATABASE_URL":  cfg.DatabaseURL,
		"S3_ENDPOINT":   cfg.S3Endpoint,
		"S3_BUCKET":     cfg.S3Bucket,
		"S3_ACCESS_KEY": cfg.S3AccessKey,
		"S3_SECRET_KEY": cfg.S3SecretKey,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		return Config{}, fmt.Errorf("missing required env vars: %v", missing)
	}
	return cfg, nil
}

func withDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func parseBool(getenv func(string) string, name string) (bool, error) {
	raw := getenv(name)
	if raw == "" {
		return false, nil
	}
	b, err := strconv.ParseBool(raw)
	if err != nil {
		return false, errors.Join(fmt.Errorf("parsing %s", name), err)
	}
	return b, nil
}
```

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/config/`
Expected: `ok  sdano.app/api/internal/config`

- [ ] **Step 6: Write `app.New` and `main.go`**

`apps/api/internal/app/app.go`:

```go
// Package app assembles the HTTP API: router, huma, middleware, and all
// route registrations. cmd/api and tests both build the app through New.
package app

import (
	"context"
	"fmt"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/danielgtaylor/huma/v2/adapters/humachi"
	"github.com/go-chi/chi/v5"
	"github.com/go-chi/chi/v5/middleware"

	"sdano.app/api/internal/config"
)

// HealthCheck is a named dependency probe run by GET /healthz.
type HealthCheck struct {
	Name string
	Ping func(ctx context.Context) error
}

// Deps carries everything app.New wires into handlers. Grows with the app.
type Deps struct {
	Checks []HealthCheck
}

type healthOutput struct {
	Body struct {
		Status string `json:"status" example:"ok" doc:"Overall service health"`
	}
}

func New(cfg config.Config, deps Deps) (*chi.Mux, huma.API) {
	router := chi.NewMux()
	router.Use(middleware.RequestID)
	router.Use(middleware.RealIP)

	humaCfg := huma.DefaultConfig("Sdano API", "0.1.0")
	humaCfg.Info.Description = "Photo-evidence and reporting platform for field service contractors."
	humaCfg.DocsPath = "/docs"
	api := humachi.New(router, humaCfg)

	huma.Register(api, huma.Operation{
		OperationID: "healthz",
		Method:      http.MethodGet,
		Path:        "/healthz",
		Summary:     "Service health",
		Tags:        []string{"meta"},
	}, func(ctx context.Context, _ *struct{}) (*healthOutput, error) {
		for _, c := range deps.Checks {
			if err := c.Ping(ctx); err != nil {
				return nil, huma.Error503ServiceUnavailable(
					fmt.Sprintf("dependency %s unavailable", c.Name))
			}
		}
		out := &healthOutput{}
		out.Body.Status = "ok"
		return out, nil
	})

	return router, api
}
```

`apps/api/cmd/api/main.go`:

```go
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
)

func main() {
	logger := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	slog.SetDefault(logger)

	if len(os.Args) > 1 && os.Args[1] == "openapi" {
		if err := printOpenAPI(); err != nil {
			logger.Error("emitting openapi", "error", err)
			os.Exit(1)
		}
		return
	}

	if err := run(logger); err != nil {
		logger.Error("api exited", "error", err)
		os.Exit(1)
	}
}

// printOpenAPI builds the app with zero deps (handlers register but never
// run) and dumps the OpenAPI 3.1 spec to stdout for orval.
func printOpenAPI() error {
	_, api := app.New(config.Config{}, app.Deps{})
	b, err := json.MarshalIndent(api.OpenAPI(), "", "  ")
	if err != nil {
		return fmt.Errorf("marshaling openapi spec: %w", err)
	}
	_, err = os.Stdout.Write(append(b, '\n'))
	return err
}

func run(logger *slog.Logger) error {
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return fmt.Errorf("loading config: %w", err)
	}

	router, _ := app.New(cfg, app.Deps{})

	srv := &http.Server{
		Addr:              cfg.HTTPAddr,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	errCh := make(chan error, 1)
	go func() {
		logger.Info("api listening", "addr", cfg.HTTPAddr)
		errCh <- srv.ListenAndServe()
	}()

	select {
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(shutdownCtx); err != nil {
			return fmt.Errorf("graceful shutdown: %w", err)
		}
		logger.Info("api stopped")
		return nil
	}
}
```

`apps/api/.golangci.yml`:

```yaml
version: "2"
linters:
  default: standard
```

- [ ] **Step 7: Verify build, lint, and a live smoke run**

```bash
go build ./... && golangci-lint run
```

Expected: no output (clean build, zero warnings). If `golangci-lint` is not installed locally: `curl -sSfL https://golangci-lint.run/install.sh | sh -s -- -b $(go env GOPATH)/bin v2.12.2`.

```bash
set -a; source ../../.env; set +a
go run ./cmd/api &
sleep 1
curl -s http://localhost:8080/healthz
kill %1
```

Expected: `{"$schema":...,"status":"ok"}` and a JSON slog line `api listening`.

- [ ] **Step 8: Commit**

```bash
git add apps/api
git commit -m "feat: api skeleton — env config, huma+chi server, /healthz, scalar docs"
```

---

### Task 6: sqlc pipeline — first generated query

**Files:**
- Create: `sqlc.yaml` (repo root)
- Create: `db/queries/object.sql`
- Generated: `apps/api/internal/db/` (committed, never hand-edited)

**Interfaces:**
- Consumes: schema from Task 4; module from Task 5.
- Produces: package `sdano.app/api/internal/db` with `db.New(pool)` → `*db.Queries` and the method `ListObjects(ctx context.Context, tenantID uuid.UUID) ([]Object, error)` where `Object` has fields `ID uuid.UUID, Name string, Address *string, Lat *float64, Lon *float64, Kind *string, QrToken *string, ContractID uuid.NullUUID, IsActive bool, CreatedAt time.Time` (exact shapes come from sqlc output — trust the generated file, not this list, if they differ).

- [ ] **Step 1: Install sqlc and pull runtime deps**

```bash
go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
cd apps/api && go get github.com/jackc/pgx/v5@latest && go get github.com/google/uuid@latest
```

- [ ] **Step 2: Create `sqlc.yaml` at the repo root**

```yaml
version: "2"
sql:
  - engine: "postgresql"
    queries: "db/queries"
    schema: "db/migrations"
    gen:
      go:
        package: "db"
        out: "apps/api/internal/db"
        sql_package: "pgx/v5"
        emit_pointers_for_null_types: true
        overrides:
          - db_type: "uuid"
            go_type: "github.com/google/uuid.UUID"
          - db_type: "uuid"
            nullable: true
            go_type: "github.com/google/uuid.NullUUID"
          - db_type: "citext"
            go_type: "string"
          - db_type: "citext"
            nullable: true
            go_type:
              type: "string"
              pointer: true
```

(sqlc reads golang-migrate-style `*.up.sql` files as schema and ignores the `*.down.sql` files.)

- [ ] **Step 3: Create `db/queries/object.sql`**

```sql
-- name: ListObjects :many
SELECT id, name, address, lat, lon, kind, qr_token, contract_id, is_active, created_at
FROM object
WHERE tenant_id = $1
  AND is_active
ORDER BY name;
```

- [ ] **Step 4: Generate and verify it compiles**

```bash
make generate-sqlc
cd apps/api && go build ./...
```

Expected: `apps/api/internal/db/` now contains `db.go`, `models.go`, `object.sql.go`; build is clean. Inspect `object.sql.go` — it must contain `func (q *Queries) ListObjects(ctx context.Context, tenantID uuid.UUID)`.

- [ ] **Step 5: Commit (generated code included)**

```bash
git add sqlc.yaml db/queries/ apps/api/internal/db/ apps/api/go.mod apps/api/go.sum
git commit -m "feat: sqlc pipeline with first query (ListObjects)"
```

---

### Task 7: Real dependencies — pgx pool, S3 client, live /healthz, test DB helper

**Files:**
- Create: `apps/api/internal/app/deps.go`
- Create: `apps/api/internal/testdb/testdb.go`
- Test: `apps/api/internal/app/health_test.go`
- Modify: `apps/api/cmd/api/main.go` (wire pool + S3 into `run`)

**Interfaces:**
- Consumes: `config.Config`, `app.Deps`/`app.HealthCheck` from Task 5.
- Produces:
  - `app.NewPool(ctx, cfg) (*pgxpool.Pool, error)` and `app.NewS3(cfg) *s3.Client` in `deps.go`.
  - `app.DBCheck(pool *pgxpool.Pool) HealthCheck`, `app.S3Check(client *s3.Client, bucket string) HealthCheck`.
  - `testdb.New(t *testing.T) *pgxpool.Pool` — starts a `postgres:18.4` testcontainer, applies all migrations, returns a connected pool (Task 8 and every future integration test uses this).
  - `Deps` gains fields: `Pool *pgxpool.Pool`.

- [ ] **Step 1: Pull dependencies**

```bash
cd apps/api
go get github.com/aws/aws-sdk-go-v2/config@latest \
       github.com/aws/aws-sdk-go-v2/credentials@latest \
       github.com/aws/aws-sdk-go-v2/service/s3@latest
go get github.com/testcontainers/testcontainers-go@latest \
       github.com/testcontainers/testcontainers-go/modules/postgres@latest
go get github.com/golang-migrate/migrate/v4@latest
```

- [ ] **Step 2: Write `apps/api/internal/testdb/testdb.go`**

```go
// Package testdb boots a disposable PostgreSQL container with the full
// schema applied — the fixture for every integration test in the repo.
package testdb

import (
	"context"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/pgx/v5" // pgx5:// driver
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	tcpostgres "github.com/testcontainers/testcontainers-go/modules/postgres"
)

// New starts postgres:18.4, applies db/migrations, and returns a pool.
// The container and pool are cleaned up with the test.
func New(t *testing.T) *pgxpool.Pool {
	t.Helper()
	ctx := context.Background()

	container, err := tcpostgres.Run(ctx, "postgres:18.4",
		tcpostgres.WithDatabase("sdano_test"),
		tcpostgres.WithUsername("sdano"),
		tcpostgres.WithPassword("sdano"),
		tcpostgres.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("starting postgres container: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = container.Terminate(ctx)
	})

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("container dsn: %v", err)
	}

	m, err := migrate.New("file://"+migrationsDir(t), strings.Replace(dsn, "postgres://", "pgx5://", 1))
	if err != nil {
		t.Fatalf("opening migrations: %v", err)
	}
	if err := m.Up(); err != nil {
		t.Fatalf("applying migrations: %v", err)
	}
	srcErr, dbErr := m.Close()
	if srcErr != nil || dbErr != nil {
		t.Fatalf("closing migrator: src=%v db=%v", srcErr, dbErr)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("connecting pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

// migrationsDir resolves <repo-root>/db/migrations from this file's location,
// so tests work regardless of the working directory.
func migrationsDir(t *testing.T) string {
	t.Helper()
	_, self, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolving caller path")
	}
	// self = <root>/apps/api/internal/testdb/testdb.go
	return filepath.Join(filepath.Dir(self), "..", "..", "..", "..", "db", "migrations")
}
```

- [ ] **Step 3: Write the failing health integration test**

`apps/api/internal/app/health_test.go`:

```go
package app_test

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

func TestHealthzReportsDBState(t *testing.T) {
	pool := testdb.New(t)

	router, _ := app.New(config.Config{}, app.Deps{
		Pool:   pool,
		Checks: []app.HealthCheck{app.DBCheck(pool)},
	})

	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("healthy DB: got %d, want 200; body: %s", rec.Code, rec.Body)
	}

	pool.Close()
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed DB: got %d, want 503; body: %s", rec.Code, rec.Body)
	}
}
```

- [ ] **Step 4: Run the test to verify it fails**

Run: `go test ./internal/app/`
Expected: FAIL — `undefined: app.DBCheck` and `unknown field Pool` (compile errors).

- [ ] **Step 5: Implement `deps.go` and extend `Deps`**

`apps/api/internal/app/deps.go`:

```go
package app

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/config"
)

func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("creating pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

func NewS3(cfg config.Config) *s3.Client {
	awsCfg, _ := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.S3Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
	)
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = &cfg.S3Endpoint
		o.UsePathStyle = cfg.S3UsePathStyle
	})
}

func DBCheck(pool *pgxpool.Pool) HealthCheck {
	return HealthCheck{Name: "postgres", Ping: pool.Ping}
}

func S3Check(client *s3.Client, bucket string) HealthCheck {
	return HealthCheck{Name: "s3", Ping: func(ctx context.Context) error {
		_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
		if err != nil {
			return fmt.Errorf("head bucket %s: %w", bucket, err)
		}
		return nil
	}}
}
```

In `app.go`, extend `Deps`:

```go
// Deps carries everything app.New wires into handlers. Grows with the app.
type Deps struct {
	Pool   *pgxpool.Pool
	Checks []HealthCheck
}
```

(add the `"github.com/jackc/pgx/v5/pgxpool"` import to `app.go`).

- [ ] **Step 6: Run the test to verify it passes**

Run: `go test ./internal/app/` (Docker must be running — testcontainers)
Expected: PASS (first run pulls the postgres:18.4 image; allow a minute).

- [ ] **Step 7: Wire real deps in `main.go`**

In `run(logger)`, replace `router, _ := app.New(cfg, app.Deps{})` with:

```go
	pool, err := app.NewPool(ctx, cfg)
	if err != nil {
		return err
	}
	defer pool.Close()

	s3c := app.NewS3(cfg)

	router, _ := app.New(cfg, app.Deps{
		Pool: pool,
		Checks: []app.HealthCheck{
			app.DBCheck(pool),
			app.S3Check(s3c, cfg.S3Bucket),
		},
	})
```

and move the `signal.NotifyContext` line above it (the pool needs `ctx`). Then smoke-verify against real infra:

```bash
set -a; source ../../.env; set +a
go run ./cmd/api &
sleep 1
curl -s http://localhost:8080/healthz    # {"status":"ok"} — postgres+minio up
kill %1
```

- [ ] **Step 8: Lint, test all, commit**

```bash
golangci-lint run && go test ./...
git add apps/api
git commit -m "feat: live healthz — pgx pool, s3 client, testcontainers helper"
```

---

### Task 8: Tracer endpoint — /api/v1/staff/objects with dev-only tenant auth

**Files:**
- Create: `apps/api/internal/auth/principal.go`
- Create: `apps/api/internal/object/http.go`
- Test: `apps/api/internal/object/http_test.go`
- Modify: `apps/api/internal/app/app.go` (register middleware + routes)

**Interfaces:**
- Consumes: `db.Queries.ListObjects` (Task 6), `testdb.New` (Task 7), `Deps.Pool`.
- Produces:
  - `auth.Principal{UserID, TenantID uuid.UUID; Role string}`, `auth.PrincipalFrom(ctx) (Principal, bool)`, `auth.NewDevTenantHeader(api huma.API) func(huma.Context, func(huma.Context))` — the huma middleware the real auth plan will replace.
  - Route `GET /api/v1/staff/objects` → `{"objects": [...]}` — the tracer the api-client is generated from.

- [ ] **Step 1: Write the failing integration test**

`apps/api/internal/object/http_test.go`:

```go
package object_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

func TestListObjectsIsTenantScoped(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	var tenantA, tenantB uuid.UUID
	for _, row := range []struct {
		id   *uuid.UUID
		name string
	}{{&tenantA, "A"}, {&tenantB, "B"}} {
		*row.id = uuid.New()
		if _, err := pool.Exec(ctx,
			`INSERT INTO tenant (id, name) VALUES ($1, $2)`, *row.id, row.name); err != nil {
			t.Fatalf("insert tenant %s: %v", row.name, err)
		}
	}
	mustExec := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	mustExec(`INSERT INTO object (tenant_id, name) VALUES ($1, 'Lenina 45 — bus stop')`, tenantA)
	mustExec(`INSERT INTO object (tenant_id, name) VALUES ($1, 'Other tenant object')`, tenantB)
	mustExec(`INSERT INTO object (tenant_id, name, is_active) VALUES ($1, 'Retired stop', false)`, tenantA)

	cfg := config.Config{DevTenantHeaderAuth: true}
	router, _ := app.New(cfg, app.Deps{Pool: pool})

	get := func(headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
		for k, v := range headers {
			req.Header.Set(k, v)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}

	// No auth header → 401 problem+json.
	if rec := get(nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("no header: got %d, want 401; body: %s", rec.Code, rec.Body)
	}

	// Tenant A sees exactly its one active object.
	rec := get(map[string]string{"X-Dev-Tenant-Id": tenantA.String()})
	if rec.Code != http.StatusOK {
		t.Fatalf("tenant A: got %d; body: %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lenina 45") {
		t.Errorf("tenant A must see its object; body: %s", body)
	}
	if strings.Contains(body, "Other tenant object") {
		t.Errorf("tenant isolation broken — tenant B object leaked; body: %s", body)
	}
	if strings.Contains(body, "Retired stop") {
		t.Errorf("inactive objects must be filtered; body: %s", body)
	}
}

func TestDevAuthDisabledMeansNoAccess(t *testing.T) {
	pool := testdb.New(t)
	router, _ := app.New(config.Config{DevTenantHeaderAuth: false}, app.Deps{Pool: pool})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/staff/objects", nil)
	req.Header.Set("X-Dev-Tenant-Id", uuid.NewString())
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("dev auth off: got %d, want 401", rec.Code)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Run: `go test ./internal/object/`
Expected: FAIL — package `object` does not exist / route 404 (compile or 404 assertions).

- [ ] **Step 3: Implement the principal and dev middleware**

`apps/api/internal/auth/principal.go`:

```go
// Package auth defines the authenticated principal and the middleware that
// establishes it. The walking skeleton ships ONLY the dev header
// authenticator; the auth plan replaces it with JWT + device tokens.
package auth

import (
	"context"
	"net/http"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
)

type Role string

const (
	RoleAdmin   Role = "admin"
	RoleManager Role = "manager"
	RoleWorker  Role = "worker"
)

type Principal struct {
	UserID   uuid.UUID
	TenantID uuid.UUID
	Role     Role
}

type ctxKey struct{}

func withPrincipal(ctx context.Context, p Principal) context.Context {
	return context.WithValue(ctx, ctxKey{}, p)
}

// PrincipalFrom returns the authenticated principal established by middleware.
func PrincipalFrom(ctx context.Context) (Principal, bool) {
	p, ok := ctx.Value(ctxKey{}).(Principal)
	return p, ok
}

// NewDevTenantHeader authenticates via the X-Dev-Tenant-Id header.
// DEV ONLY (gated by DEV_TENANT_HEADER_AUTH): exists so the walking skeleton
// can exercise tenant-scoped queries before real auth lands. The auth plan
// deletes this middleware and the env flag together.
func NewDevTenantHeader(api huma.API, enabled bool) func(huma.Context, func(huma.Context)) {
	return func(ctx huma.Context, next func(huma.Context)) {
		if !enabled {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		tenantID, err := uuid.Parse(ctx.Header("X-Dev-Tenant-Id"))
		if err != nil {
			_ = huma.WriteErr(api, ctx, http.StatusUnauthorized, "authentication required")
			return
		}
		next(huma.WithContext(ctx, withPrincipal(ctx.Context(), Principal{
			TenantID: tenantID,
			Role:     RoleAdmin,
		})))
	}
}
```

- [ ] **Step 4: Implement the objects route**

`apps/api/internal/object/http.go`:

```go
// Package object exposes serviced-object endpoints. The list endpoint is the
// walking-skeleton tracer proving SQL → sqlc → huma → OpenAPI end to end.
package object

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

type Object struct {
	ID        uuid.UUID  `json:"id"`
	Name      string     `json:"name" example:"Lenina st., 45 — bus stop"`
	Address   *string    `json:"address"`
	Lat       *float64   `json:"lat"`
	Lon       *float64   `json:"lon"`
	Kind      *string    `json:"kind" example:"bus_stop"`
	QRToken   *string    `json:"qr_token"`
	IsActive  bool       `json:"is_active"`
	CreatedAt time.Time  `json:"created_at"`
}

type listOutput struct {
	Body struct {
		Objects []Object `json:"objects"`
	}
}

func Register(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "listStaffObjects",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/objects",
		Summary:     "List active objects",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, _ *struct{}) (*listOutput, error) {
		principal, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		rows, err := queries.ListObjects(ctx, principal.TenantID)
		if err != nil {
			return nil, fmt.Errorf("listing objects for tenant %s: %w", principal.TenantID, err)
		}
		out := &listOutput{}
		out.Body.Objects = make([]Object, 0, len(rows))
		for _, r := range rows {
			out.Body.Objects = append(out.Body.Objects, Object{
				ID:        r.ID,
				Name:      r.Name,
				Address:   r.Address,
				Lat:       r.Lat,
				Lon:       r.Lon,
				Kind:      r.Kind,
				QRToken:   r.QrToken,
				IsActive:  r.IsActive,
				CreatedAt: r.CreatedAt,
			})
		}
		return out, nil
	})
}
```

(If sqlc generated different field types — e.g. `pgtype.Timestamptz` for `created_at` — adapt the mapping to the generated types; never edit the generated file.)

- [ ] **Step 5: Register middleware and route in `app.New`**

In `app.go`, after `api := humachi.New(...)` add:

```go
	api.UseMiddleware(auth.NewDevTenantHeader(api, cfg.DevTenantHeaderAuth))

	if deps.Pool != nil {
		object.Register(api, db.New(deps.Pool))
	}
```

with imports `sdano.app/api/internal/auth`, `sdano.app/api/internal/db`, `sdano.app/api/internal/object`.

**Important:** the middleware runs on every huma operation including `/healthz`. Exempt health checks — in `NewDevTenantHeader`, before the `enabled` check, add:

```go
		if ctx.Operation().OperationID == "healthz" {
			next(ctx)
			return
		}
```

- [ ] **Step 6: Run all tests to verify they pass**

Run: `go test ./...`
Expected: PASS across `config`, `app`, `object` packages (health test must still pass with the exemption in place).

- [ ] **Step 7: Lint and commit**

```bash
golangci-lint run
git add apps/api
git commit -m "feat: tracer endpoint /api/v1/staff/objects with dev-only tenant auth"
```

---

### Task 9: OpenAPI → orval → packages/api-client

**Files:**
- Create: `packages/api-client/package.json`, `packages/api-client/orval.config.ts`, `packages/api-client/tsconfig.json`, `packages/api-client/src/index.ts`
- Generated: `packages/api-client/openapi.json`, `packages/api-client/src/generated/` (committed)

**Interfaces:**
- Consumes: `go run ./cmd/api openapi` (Task 5), the tracer operation `listStaffObjects` (Task 8), Makefile targets `openapi` / `generate-client` (Task 3).
- Produces: `@sdano/api-client` workspace package exporting generated types + fetch client; the drift-check contract used by CI (`make drift`).

- [ ] **Step 1: Create the package files**

`packages/api-client/package.json`:

```json
{
  "name": "@sdano/api-client",
  "version": "0.0.0",
  "private": true,
  "type": "module",
  "main": "./src/index.ts",
  "scripts": {
    "generate": "orval --config ./orval.config.ts",
    "typecheck": "tsc --noEmit"
  }
}
```

`packages/api-client/orval.config.ts`:

```ts
import { defineConfig } from 'orval';

export default defineConfig({
  sdano: {
    input: './openapi.json',
    output: {
      target: './src/generated/sdano.ts',
      client: 'fetch',
      mode: 'single',
      clean: true,
      baseUrl: '/',
    },
  },
});
```

`packages/api-client/tsconfig.json`:

```json
{
  "compilerOptions": {
    "target": "ES2022",
    "module": "ESNext",
    "moduleResolution": "bundler",
    "strict": true,
    "skipLibCheck": true,
    "noEmit": true
  },
  "include": ["src"]
}
```

`packages/api-client/src/index.ts`:

```ts
export * from './generated/sdano';
```

- [ ] **Step 2: Install orval and typescript (exact-pinned, CVE floor)**

From repo root:

```bash
npm install -D -E -w packages/api-client orval@latest typescript@latest
node -e "const v=require('./node_modules/orval/package.json').version; if (v < '8.0.3') { throw new Error('orval ' + v + ' < 8.0.3 — CVE-2026-24132'); } console.log('orval', v, 'ok')"
```

Expected: `orval 8.x.y ok` (must be ≥ 8.0.3 — earlier 8.x versions have the mock-generation RCE CVE-2026-24132).

- [ ] **Step 3: Generate spec and client**

```bash
make generate-client
```

Expected: `packages/api-client/openapi.json` written (contains `"listStaffObjects"` and `"healthz"` operations); `packages/api-client/src/generated/sdano.ts` written with a `listStaffObjects` function and `Object`/`ListStaffObjectsResponse`-style types.

Run: `npm run typecheck -w packages/api-client`
Expected: no errors — the pipeline SQL → sqlc → Go → huma → OpenAPI → orval → TS holds.

- [ ] **Step 4: Verify the drift check catches drift**

```bash
make drift
```

Expected: exits 0 (regeneration is byte-identical to what's committed... first run: everything is new, so `git add` first). Concretely:

```bash
git add packages/api-client
make drift        # regenerate + git diff --exit-code → clean
```

- [ ] **Step 5: Commit**

```bash
git add packages/api-client package.json package-lock.json
git commit -m "feat: orval-generated typescript api client with drift check"
```

---

### Task 10: API Dockerfile + compose api service + Caddyfile stub

**Files:**
- Create: `deploy/Dockerfile.api`, `deploy/Caddyfile`
- Modify: `deploy/docker-compose.yml` (add `api` service to dev profile, `caddy` to prod profile)

**Interfaces:**
- Consumes: the buildable `cmd/api` from Tasks 5–8; compose infra from Task 3.
- Produces: `docker compose --profile dev up` runs the full dev stand including the API on `:8080` — the spec's success criterion #1.

- [ ] **Step 1: Create `deploy/Dockerfile.api`**

```dockerfile
FROM golang:1.26.5-alpine AS build
WORKDIR /src
COPY apps/api/go.mod apps/api/go.sum ./
RUN go mod download
COPY apps/api/ ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o /out/api ./cmd/api

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/api /api
EXPOSE 8080
ENTRYPOINT ["/api"]
```

- [ ] **Step 2: Create `deploy/Caddyfile` (prod stub — completed in the deployment plan)**

```
# Production reverse proxy (docs/10-deployment.md). Used by the prod profile only.
{$API_DOMAIN:api.localhost} {
	reverse_proxy api:8080
}
```

- [ ] **Step 3: Add services to `deploy/docker-compose.yml`**

Append to `services:`:

```yaml
  api:
    build:
      context: ..
      dockerfile: deploy/Dockerfile.api
    profiles: [dev]
    environment:
      HTTP_ADDR: ${HTTP_ADDR}
      DATABASE_URL: postgres://${POSTGRES_USER}:${POSTGRES_PASSWORD}@postgres:5432/${POSTGRES_DB}?sslmode=disable
      S3_ENDPOINT: http://minio:9000
      S3_REGION: ${S3_REGION}
      S3_BUCKET: ${S3_BUCKET}
      S3_ACCESS_KEY: ${S3_ACCESS_KEY}
      S3_SECRET_KEY: ${S3_SECRET_KEY}
      S3_USE_PATH_STYLE: ${S3_USE_PATH_STYLE}
      ADMIN_ORIGIN: ${ADMIN_ORIGIN}
      DEV_TENANT_HEADER_AUTH: ${DEV_TENANT_HEADER_AUTH}
    ports:
      - "8080:8080"
    depends_on:
      postgres:
        condition: service_healthy
      minio-setup:
        condition: service_completed_successfully

  caddy:
    image: caddy:latest
    profiles: [prod]
    ports:
      - "80:80"
      - "443:443"
    volumes:
      - ./Caddyfile:/etc/caddy/Caddyfile:ro
      - caddy_data:/data
```

and add `caddy_data:` to the `volumes:` block.

- [ ] **Step 4: Smoke-test the full dev stand**

```bash
make dev-up && make migrate-up
curl -s http://localhost:8080/healthz
curl -s http://localhost:8080/docs | head -c 200
```

Expected: healthz returns `{"status":"ok"}` (API in Docker reaching postgres + minio); `/docs` returns the Scalar HTML page.

- [ ] **Step 5: Commit**

```bash
git add deploy/
git commit -m "feat: api dockerfile, compose api service, caddy prod stub"
```

---

### Task 11: CI — GitHub Actions

**Files:**
- Create: `.github/workflows/ci.yml`

**Interfaces:**
- Consumes: everything above (lint, tests, sqlc, orval, docker build).
- Produces: the merge gate. Runs on pushes to `main` and all PRs.

- [ ] **Step 1: Create `.github/workflows/ci.yml`**

```yaml
name: ci

on:
  push:
    branches: [main]
  pull_request:

jobs:
  go:
    runs-on: ubuntu-latest
    defaults:
      run:
        working-directory: apps/api
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26.5"
          cache-dependency-path: apps/api/go.sum
      - name: Lint (zero warnings policy)
        uses: golangci/golangci-lint-action@v8
        with:
          version: v2.12.2
          working-directory: apps/api
      - name: Tests (includes testcontainers integration)
        run: go test ./...
      - name: govulncheck (version policy gate)
        run: |
          go install golang.org/x/vuln/cmd/govulncheck@latest
          govulncheck ./...

  codegen-drift:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - uses: actions/setup-go@v5
        with:
          go-version: "1.26.5"
          cache-dependency-path: apps/api/go.sum
      - uses: actions/setup-node@v4
        with:
          node-version: "24"
          cache: npm
      - name: Install workspace deps
        run: npm ci
      - name: npm audit (version policy gate)
        run: npm audit --audit-level=high
      - name: sqlc drift check
        run: |
          go install github.com/sqlc-dev/sqlc/cmd/sqlc@v1.31.1
          sqlc generate
          git diff --exit-code
      - name: OpenAPI + orval drift check
        run: |
          make openapi
          npm run generate -w packages/api-client
          git diff --exit-code

  docker:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v5
      - name: Build API image
        run: docker build -f deploy/Dockerfile.api .
```

Note (version policy): action majors (`checkout@v5`, `setup-go@v5`, `setup-node@v4`, `golangci-lint-action@v8`) are the latest known-good as of plan writing — bump to the current majors at execution if newer exist.

- [ ] **Step 2: Validate the workflow syntax**

Run: `docker run --rm -v $(pwd):/repo -w /repo rhysd/actionlint:latest`
Expected: no output (clean). If the image is unavailable, `npx -y yaml-lint .github/workflows/ci.yml` as a syntax-only fallback.

- [ ] **Step 3: Commit**

```bash
git add .github/
git commit -m "ci: lint, tests, govulncheck, sqlc/orval drift checks, docker build"
```

---

### Task 12: Final verification sweep

**Files:** none new — verification only.

**Interfaces:**
- Consumes: everything.
- Produces: the walking skeleton declared done against the spec's success criteria.

- [ ] **Step 1: Clean-room run of the whole gate**

```bash
make dev-down && make dev-up && make migrate-up
make lint && make test && make drift
curl -s http://localhost:8080/healthz
```

Expected: every command exits 0; healthz `{"status":"ok"}`.

- [ ] **Step 2: End-to-end tracer proof with real data**

```bash
TENANT=$(docker compose -f deploy/docker-compose.yml --env-file .env exec -T postgres \
  psql -U sdano -d sdano -tAc \
  "INSERT INTO tenant (name) VALUES ('Walking Skeleton Demo') RETURNING id")
docker compose -f deploy/docker-compose.yml --env-file .env exec -T postgres \
  psql -U sdano -d sdano -c \
  "INSERT INTO object (tenant_id, name, address) VALUES ('$TENANT', 'Lenina st., 45 — bus stop', 'Lenina 45')"
curl -s -H "X-Dev-Tenant-Id: $TENANT" http://localhost:8080/api/v1/staff/objects
```

Expected: JSON with one object named `Lenina st., 45 — bus stop` — data born in SQL, served through sqlc → huma, described in `openapi.json`, typed in `packages/api-client`.

- [ ] **Step 3: Verify the working tree is clean and history is sound**

```bash
git status --short   # empty
git log --oneline    # conventional commits, one concern each
```

- [ ] **Step 4: Mark plan complete**

Report to the user: the skeleton is proven; the next plan is **auth** (spec phase 3), which replaces `auth.NewDevTenantHeader` with real staff JWT + worker device tokens and deletes `DEV_TENANT_HEADER_AUTH`.

---

## Plan Self-Review (done at write time)

- **Spec coverage (phases 1–2):** agent docs ✓ (T1), git ✓ (done in brainstorming + T1), Nx + layout ✓ (T1/T2), full schema + doc merge ✓ (T4), sqlc ✓ (T6), huma + Scalar ✓ (T5), orval + api-client ✓ (T9), compose dev ✓ (T3/T10), CI with all four gates ✓ (T11), success criteria ✓ (T12). Out of plan scope by design: auth, worker/staff feature APIs, reports, seed-demo, sdano-ops (follow-up plans).
- **Known judgment calls:** dev-only header auth is explicitly temporary and env-gated (deleted by the auth plan); `headless-shell` and `minio` images are `latest` in dev only — pinned at first real use per version policy.
- **Type consistency:** `config.Config` fields, `app.Deps{Pool, Checks}`, `app.HealthCheck{Name, Ping}`, `auth.PrincipalFrom`, `db.Queries.ListObjects(ctx, uuid.UUID)` used consistently across Tasks 5–10.
