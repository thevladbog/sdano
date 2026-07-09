# AGENTS.md — Instructions for AI Coding Agents

You are working on **Sdano** — a photo-evidence and reporting platform for field service contractors. Read this file fully before making changes. When in doubt, the documentation in `docs/` is the source of truth; this file is the operational digest.

## Project context in 60 seconds

- Field workers (cheap Androids, gloves, no signal) complete checklists on physical objects and prove work with geotagged photos. Owners generate dispute-grade PDF reports for municipal clients. Full picture: `docs/01-concept.md`.
- Solo-developer project with a hard constraint: **evenings-and-weekends time budget**. Simplicity beats sophistication in every tradeoff.
- The codebase is written **as if public** (a planned open-source pivot): no secrets in code, no shortcuts you'd be embarrassed to publish.

## Golden rules (violating these fails review)

1. **Evidence is sacred.** Photos, executions, and generated reports are immutable, insert-only, never silently dropped. Any code path that could lose or alter evidence must fail loudly instead. Missing data renders as an explicit placeholder, never as a silent skip.
2. **Idempotency everywhere on the mobile-facing API.** All mobile mutations are full-state upserts keyed by client-generated UUIDs. Replaying any request any number of times must converge to the same state. Never introduce a mobile-facing endpoint that violates this.
3. **The type pipeline is law: SQL → sqlc → Go → huma → OpenAPI → orval → TS.** Never hand-write types that the pipeline can generate. Never edit generated files (`packages/api-client/`, sqlc output) — regenerate them. CI fails on drift.
4. **tenant_id on every domain query.** Every sqlc query touching domain tables takes tenant_id from the authenticated principal. No exceptions, no "it's just an internal endpoint."
5. **Offline-first is not optional.** Mobile features must work with zero connectivity: local SQLite state + outbox job. If a feature can't work offline, that's a design conversation, not an implementation detail. See `docs/08-offline-sync.md`.
6. **PII minimalism.** Never add fields collecting personal data (emails, phones, precise home addresses) without an explicit requirement. Workers are a display name and an invite code.
7. **No new heavy dependencies without justification.** No ORM, no sync frameworks, no Kubernetes, no auth SaaS (legally prohibited — 152-FZ data residency). Prefer the standard library and the existing stack. If you believe a dependency is warranted, state the rationale in the PR description.

## Stack quick reference

| Area | Use | Never use |
|---|---|---|
| Backend | Go, chi router, huma (handlers = typed structs) | gin/echo/fiber, ORMs (GORM etc.) |
| DB access | sqlc queries in `db/queries/*.sql`, pgx/v5 | raw string SQL in Go code, query builders |
| Migrations | plain SQL in `db/migrations/`, backward-compatible by policy | ORM auto-migrations |
| Admin | Next.js (TypeScript), orval-generated clients | hand-rolled fetch types |
| Mobile | Expo/React Native, expo-sqlite, orval clients | native modules unless unavoidable, localStorage-style hacks |
| Files | S3 via AWS SDK, presigned URLs; API never streams bytes | minio-specific client, files through the API |
| Auth | in-repo implementation (argon2id, JWT+refresh for staff, device tokens for workers) | Clerk/Auth0/Keycloak |
| PDF | Go html/template → headless Chrome (chromedp) | PDF-construction libraries |

## Workflow expectations

- **Before coding:** read the relevant doc (`docs/06` for schema work, `docs/07` for API, `docs/08` for anything mobile-sync, `docs/09` for reports). If your change contradicts a doc, update the doc in the same PR — docs and code never diverge silently.
- **Migrations:** additive and backward-compatible (code version N must run on schema N+1). Never edit an applied migration; add a new one. Every migration has a working `down`.
- **Generated code:** after changing `db/queries/*.sql` run sqlc; after changing API handler types, regenerate the OpenAPI spec and orval clients. Commit generated output.
- **Dependencies:** adopt the latest stable, security-clean version; check the current version and API via context7 before adding or upgrading; pin exactly (`go.sum`, `package-lock.json`). `govulncheck` and `npm audit` gate CI.
- **Tests:** the offline sync component and idempotency guarantees have priority test coverage (property-style: replay any prefix of the outbox in any order → same DB state). Don't add tests for trivial CRUD just for coverage numbers; do add a test for every bug you fix.
- **Errors:** RFC 7807 problem+json with stable `type` slugs on the API. In Go, wrap with context (`fmt.Errorf("presigning photo %s: %w", ...)`); no naked error returns across package boundaries.
- **Logging:** structured slog, no fmt.Println. Never log photo URLs with credentials, tokens, or invite codes.
- **Commits:** conventional commits (`feat:`, `fix:`, `docs:`, `chore:`); small, single-purpose. The history is part of the public-ready discipline.

## Things that look like improvements but are deliberate decisions — do not "fix"

- **No recurrence/schedule table** — work orders are pre-generated (docs/06, decision 8). Don't invent an RRULE engine.
- **Last-write-wins conflict handling** — sufficient because one worker owns one execution (docs/08). Don't add CRDTs, vector clocks, or merge UIs.
- **Full-refresh `GET /worker/today`** instead of delta sync — the working set is a day's route; simplicity wins (docs/08).
- **No self-service registration, no payments integration** — onboarding and invoicing are manual at this stage (docs/01).
- **Single Go binary, single VPS, docker-compose** — no service extraction, no k8s manifests (docs/10).
- **Device time and server time both stored** (`device_finished_at`) — not redundancy, a dispute-evidence requirement (docs/06, decision 2).
- **Reports regenerate as new immutable rows** — never "update" an existing PDF (docs/09).
- **No payment provider integration, no dunning automation** — billing is manual invoices + `billed_until` (docs/12).
- **No plan-limit enforcement in code** — limits live in `plan_note` and human conversation; an over-limit client is a sales call, not a 403 (docs/12).
- **No operator web UI in Phase A** — the operator surface is the `sdano-ops` CLI over SSH; the web app knows only tenant-scoped principals (docs/12).
- **Suspension never blocks access to collected evidence** — suspended tenants are read-only, never locked out of their photos and past reports; pre-suspension work in a worker's outbox is always accepted (docs/12).

## Language and localization

- Code, comments, commits, docs: **English**.
- User-facing strings: Russian first (the paying market), externalized from day one (no hardcoded UI strings). The RU wording uses the client's world: «Объект», «Сдано», «Заявка» — never transliterated tech jargon.
- The brand stamp exists in two versions: СДАНО (RU product) and SDANO (international/repo contexts).

## Definition of done for any task

1. Works offline if it's a mobile feature (tested with airplane mode).
2. Generated code regenerated and committed; CI green (lint, tests, sqlc check, orval drift check).
3. Relevant doc updated if behavior/contract changed.
4. No new PII, no new heavy deps, tenant isolation intact.
5. Migration (if any) is additive and reversible.
