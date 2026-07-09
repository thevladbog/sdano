# Sdano — Backend (Slice 0 + Slice 1) Design

**Date:** 2026-07-09
**Status:** approved (brainstorming session with the owner)
**Normative context:** this spec builds on `docs/01`–`docs/12`, `AGENTS.md`, and `11-development-rules.md`. Where those documents already define a contract (API shapes, DDL, sync semantics, ops rules), this spec references them instead of duplicating. Anything stated here that extends those documents must be merged back into them in the same PR that implements it.

## Goal

Stand up the entire backend needed for the slice-1 demo: monorepo foundation, full database schema, auth, the worker-facing API (offline-first, idempotent), the staff-facing API, and asynchronous PDF report generation — built walking-skeleton first so the type pipeline and CI are proven before feature work begins.

## Scope

**In scope**
- Agent-docs additions: dependency/version policy, `CLAUDE.md`, git initialization.
- Nx monorepo with backend-only projects: `apps/api` (Go), `packages/api-client` (orval-generated TS), `db/`, `deploy/`.
- Full DB schema for slices 0–2 exactly as in `docs/06-data-model.md` (issues tables included in the schema even though their API is out of scope), plus the schema additions listed below.
- Auth: staff email+password (argon2id, 15-min JWT + rotated opaque refresh), worker invite-code claim → long-lived device token.
- Worker API: `GET /worker/today`, `PUT /worker/executions/{id}`, photo presign/confirm, `GET /worker/objects/by-qr/{qr_token}` — contracts per `docs/07-api-spec.md`.
- Staff API: dashboard, objects CRUD + card, work orders (bulk create, patch), workers & invites, executions/photos reads, reports (async, 202 + polling).
- PDF report worker per `docs/09-pdf-report.md` (Go html/template → chromedp → S3).
- Background jobs (same binary): nightly `missed` marking, orphan-photo GC, report renderer.
- `sdano-ops` CLI, Phase-A minimum: `tenant create | list | suspend | activate | set-billing`, all mutations audited in `ops_audit`.
- docker-compose (dev profile: postgres, minio, api, headless-shell), CI (GitHub Actions), `make seed-demo`.

**Out of scope (deferred, not forgotten)**
- Issues API and issues PDF section (slice 2), public QR (slice 4).
- `apps/admin` (Next.js) and `apps/mobile` (Expo) implementations — the directories exist as placeholders in the root layout (see Repository structure), but the apps themselves are scaffolded and developed in later phases (empty skeletons would rot unused while versions move on).
- Checklist builder UI, recurrence engine, payments, push — per the deliberate non-features list in `AGENTS.md`.
- Production VPS provisioning (compose prod profile is written; actually deploying is a separate task).

## Deliverable 1 — agent documentation updates

1. **Dependency & version policy** — new section in `11-development-rules.md`, digest line in `AGENTS.md`:
   - Every dependency is adopted at its **latest stable, security-clean version at the time of adoption**. Before adding or upgrading a package, the agent verifies the current version and API against live documentation (context7), and checks vulnerabilities: `govulncheck` for Go, `npm audit` for TS — both run in CI.
   - Versions are pinned exactly (`go.mod`, `package-lock.json`). Upgrades land as dedicated PRs (renovate later, as already planned in §7 of the rules).
2. **`CLAUDE.md`** at the repo root — a thin pointer: read `AGENTS.md` fully before any change; `docs/` is the source of truth.
3. **Git initialization** — `git init`, branch `main`, first commit is the existing documentation; conventional commits from then on (the rules in §1 apply from the first commit).

## Repository structure

The root carries the full target monorepo layout from the README from day one — backend, frontend, deploy, docs. Frontend apps exist as reserved placeholders (a README stating "created in the frontend phase") rather than scaffolded Next.js/Expo projects, so the structure is fixed without dead dependencies aging in unused apps.

```
sdano/
  CLAUDE.md
  AGENTS.md
  11-development-rules.md
  apps/admin/              # placeholder: Next.js admin panel (frontend phase)
  apps/mobile/             # placeholder: Expo worker app (frontend phase)
  apps/api/
    cmd/api/main.go          # wires everything; single deployable binary
    cmd/ops/main.go          # sdano-ops CLI (reuses internal packages)
    internal/auth/           # principals, tokens, argon2id, middleware
    internal/tenant/         # tenant lifecycle, status gate
    internal/object/
    internal/workorder/      # orders + executions
    internal/photo/          # presign/confirm, S3, GC
    internal/report/         # aggregate queries, templates/, renderer
    internal/platform/       # ops_audit, seed, scheduler
  packages/api-client/       # orval output — never hand-edited
  db/migrations/             # plain SQL up/down (golang-migrate)
  db/queries/                # *.sql sources for sqlc
  db/seed/
  deploy/docker-compose.yml
  deploy/Caddyfile
  docs/
```

Nx orchestrates `build / test / lint / generate` targets via run-commands executors; the Go toolchain does the real work (per `docs/02`).

**Key libraries** (exact versions resolved via context7 at phase-1 start and pinned): huma v2 + chi v5 (humachi adapter), sqlc + pgx v5, golang-migrate, golang-jwt v5, `x/crypto/argon2`, aws-sdk-go-v2 (S3 + presign), chromedp, stdlib `slog`; orval for the TS client; PostgreSQL latest stable (docs require 16+).

## Schema additions (to be merged into `docs/06-data-model.md` in the implementing PR)

- **`refresh_token`** table: `id uuid PK, tenant_id, user_id, token_hash text, created_at, used_at, revoked_at, expires_at`. Opaque 256-bit tokens, SHA-256 hash at rest, 30-day TTL, rotated on every refresh; reuse of a spent token revokes the whole chain (theft detection).
- **`report.status`** enum `('generating','ready','failed')` + `report.failure_reason text`. The report rows themselves are the render queue; no separate jobs table.

Everything else ships exactly as written in `docs/06`, including indexes and `ops_audit`.

## Auth and middleware chain

Contracts per `docs/07`; mechanics:

```
request → request-id + slog context
        → rate limit (in-memory x/time/rate keyed by token; stricter on /auth/*)
        → authenticate (staff JWT | worker device token → principal{user_id, tenant_id, role})
        → tenant status gate (docs/12 table; suspended = read-only,
          EXCEPT execution flushes with device_finished_at < suspension time)
        → handler
```

- The principal lives in the request context; **every sqlc domain query takes `tenant_id` from it** (golden rule 4). `tenant_id` never appears in URLs or bodies.
- Staff access JWT: 15 min, HS256, secret from env. Worker device token: opaque, hashed at rest, revoked by deactivating the worker or token. Worker invite codes: 6 digits, single-use, 72-hour TTL.
- Rate-limit initial values (tunable, not contract): `/auth/*` 10 req/min per IP; authenticated tokens 300 req/min (photo bursts are legitimate).
- Roles: `/worker/*` requires role=worker; `/staff/*` requires admin|manager (no admin/manager distinction in slice 1).
- CORS: admin origin only, from env.

## Worker API mechanics

- **`GET /worker/today`** — full working set in one payload (objects + today's orders with the pinned checklist version denormalized). Full refresh, no delta sync (deliberate, `docs/08`).
- **`PUT /worker/executions/{id}`** — full-state idempotent upsert in one transaction: upsert `work_execution` by client UUID, replace `work_execution_item` rows from the snapshot, advance `work_order.status` (`in_progress`/`done`). Validates the order belongs to the tenant and is assigned to the caller. Replaying any snapshot any number of times converges; response is 200 with the server view (photos included). `device_finished_at` stored alongside server time.
- **Photos, two-phase** — presign creates the `photo` row with `uploaded_at = NULL` and returns a 15-min presigned PUT for `tenants/{tenant}/photos/{id}.jpg`; re-presign returns a fresh URL for the same key. Confirm HEADs S3, then stamps `uploaded_at` and stores device `taken_at`/`lat`/`lon`; re-confirm returns current state. The API never streams photo bytes.
- **Orphan GC** (background): rows with `uploaded_at IS NULL` older than 14 days are deleted **only after verifying the S3 object does not exist** — evidence is never lost silently.
- **`GET /worker/objects/by-qr/{qr_token}`** — QR resolution for objects outside today's route.

## Staff API and reports

- **Dashboard** — one aggregate query: totals (done/in_progress/overdue/total) + one row per object. The 3-second screen renders from a single call.
- **Objects / work orders / workers** — per `docs/07`: bulk order create (array body — how slice-1 "pre-generated schedule" works), reinvite with optional token revocation, cursor pagination on growing lists, presigned GET (5 min) for photo reads.
- **Reports (async)** — `POST /staff/reports` inserts a `report` row with `status='generating'`, returns 202; clients poll `GET /staff/reports/{id}`; `ready` responses carry a presigned GET for the PDF.
  - Renderer: **one goroutine in the same binary**, report rows are the queue, graceful shutdown; on startup, stale `generating` rows are marked `failed` with a reason (regeneration creates a new immutable row — a referenced PDF never changes).
  - Pipeline per `docs/09`: aggregate queries → `html/template` → chromedp against a **separate `headless-shell` container** in compose (thin API image, Chrome version pinned by image tag) → PDF to S3 → row stamped `ready`. Photos downscaled to ~1200px in the render; a missing photo renders as an explicit placeholder, never a skip.
  - `make report-preview` renders templates with fixture data locally.
- **Nightly job** (same scheduler goroutine): `scheduled` orders past `due_date` → `missed` (a fact with a timestamp — reports need it).

## sdano-ops (Phase A minimum)

`cmd/ops`, run over SSH against the prod DSN. Commands: `tenant create` (tenant + first admin, prints credentials), `tenant list`, `tenant suspend|activate`, `tenant set-billing`. Reuses the domain packages and sqlc queries — no parallel logic; every mutating command writes an `ops_audit` row.

## Error handling

- RFC 7807 `problem+json` (huma native) with stable `type` slugs (`invite-code-invalid`, `tenant-suspended`, `work-order-not-assigned`, `photo-not-uploaded`, …). Slugs are API contract; renaming one is a breaking change.
- Go errors wrapped with context at every boundary; no naked cross-package `return err`.
- `slog` JSON to stdout with request-id; never log tokens, invite codes, or credentialed URLs.
- Evidence rule: no code path drops photos/executions silently — failures are loud (5xx + log); missing report data renders as an explicit placeholder.

## Testing (priority order per rules §6)

1. **Idempotency properties** (the core guarantee): against real Postgres via testcontainers-go — replaying any prefix of a worker mutation sequence, any number of times, in any order, converges to the same DB state.
2. **Auth flows**: invite claim, refresh rotation, spent-refresh reuse → chain revocation, device-token revocation, suspended-tenant gate including the pre-suspension-work acceptance nuance.
3. **Report aggregate queries** against fixtures — a wrong number in a municipal report is a client-trust incident.
4. No CRUD tests for coverage's sake; every bugfix ships a regression test.

## Implementation phases (walking skeleton → vertical slices)

1. **Repo & rules**: git init (+ docs commit), agent-docs updates (version policy, `CLAUDE.md`), Nx workspace skeleton with the full root layout (including `apps/admin` and `apps/mobile` placeholders).
2. **Skeleton proven**: migration 0001 (full schema), sqlc setup, `GET /healthz` (DB + S3 checks) + one domain read through huma → OpenAPI 3.1 → Scalar `/docs` → orval → `packages/api-client`; compose dev profile; CI (golangci-lint zero-warnings, tests, sqlc drift check, orval drift check, govulncheck).
3. **Auth**: staff login/refresh/logout, worker claim, middleware chain (rate limit, principal, tenant gate).
4. **Worker vertical**: `/worker/today` → execution upsert (+ idempotency property tests) → photo presign/confirm → QR resolve.
5. **Staff vertical**: objects, work orders, workers/invites, dashboard, execution/photo reads.
6. **Reports & ops**: templates + chromedp + async renderer, nightly missed job, orphan GC, `make seed-demo`, `sdano-ops`.

Each phase lands as a series of small conventional commits with CI green throughout. Exact dependency versions are resolved via context7 at the start of phase 1 and pinned.

## Success criteria

- `docker compose --profile dev up` + `make migrate seed-demo` yields a working API with Scalar docs at `/docs`.
- A scripted worker day (claim → today → execute → photos, replayed with duplicates and reordering) converges — the property suite passes in CI.
- `POST /staff/reports` on the seeded demo tenant produces a PDF matching `docs/09`'s structure (cover, summary, per-object sections with photo captions, explicit gaps, signature page).
- CI fails on: lint warnings, test failures, sqlc drift, orval drift, known vulnerabilities.
