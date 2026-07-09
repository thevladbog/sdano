# Sdano — Architecture

## Principles

1. **The object is the center of the model.** Planned work and issues are two event types on an object sharing the same finale: photo + geo + time + inclusion in a report.
2. **Offline-first.** The mobile client is fully functional without a network; the server is the source of truth after sync.
3. **Self-hostable.** The whole system comes up from a single docker-compose on any VM. That simultaneously covers: local dev, production in a Russian cloud (152-FZ), deployment on a client's server, and open-source readiness.
4. **One source of truth for types.** SQL → sqlc → Go types → huma → OpenAPI → orval → TS types. Type drift is caught at compile time on both sides.
5. **PII minimalism.** Don't collect data you can live without. Worker = name + invite code.
6. **Code as if public from day one.** Secrets in env, sane commits, READMEs.

## Stack

| Layer | Technology | Rationale |
|---|---|---|
| Monorepo | Nx | Shared types/utils between admin and mobile; familiar from QuokkaQ |
| Backend | Go, chi (or net/http ServeMux) + **huma** | huma: typed handlers → OpenAPI 3.1 + built-in Scalar docs; the spec cannot drift from the code |
| Database | PostgreSQL, **sqlc + pgx/v5** | Plain SQL in .sql files, generated typed code, no ORM magic |
| Migrations | golang-migrate or goose | SQL up/down files |
| API clients | **orval** from the OpenAPI spec | Typed clients for Expo and Next.js |
| Admin | Next.js (TypeScript) | Maximum-productivity zone |
| Mobile | **Expo / React Native** | Iteration speed, OTA updates (expo-updates), EAS builds; camera/geo/SQLite out of the box. KMP rejected: a new language and ecosystem would slow things by months exactly when speed matters most |
| Mobile local DB | SQLite (expo-sqlite / op-sqlite) | Source of truth on the device |
| Files | S3-compatible storage via the **AWS S3 SDK**: MinIO locally/CI, Yandex Object Storage (or another RU provider) in production | Switching providers = changing an endpoint in env |
| Auth | Hand-rolled | Clerk/Auth0 are off the table (152-FZ: Russian citizens' data stays in Russia). Argon2/bcrypt, short-lived JWT + refresh (or sessions). Workers sign in with a 6-digit invite code from the admin — no email/password |
| PDF reports | Server-side: HTML template → headless Chrome | Faster and more flexible than PDF libraries |
| Deployment | docker-compose on a VPS (RU cloud for prod) + Caddy (TLS) | No Kubernetes below ~100 clients; operational simplicity = evenings go into features |
| CI/CD | GitHub Actions | Familiar from QuokkaQ |

## Monorepo layout (sketch)

```
sdano/
  apps/
    api/          # Go: chi + huma
    admin/        # Next.js admin panel
    mobile/       # Expo
  packages/
    api-client/   # orval-generated TS clients and types
    shared/       # shared utils, constants, zod schemas
  db/
    migrations/   # SQL up/down
    queries/      # .sql for sqlc
  deploy/
    docker-compose.yml
    Caddyfile
```

Note: the Go app lives in the monorepo but builds with its own toolchain; Nx orchestrates tasks (build/test/lint) via executors or plain script targets.

## Data model (sketch — detailed in 06-data-model.md)

Multitenancy: `tenant_id` on all domain tables; isolation at the query level (every sqlc query is parameterized by tenant_id). RLS — optional later.

Key entities:

- **tenant** — the client organization.
- **user** — admin/manager (email + password) and worker (name + invite code). Role in a role field. PII minimum.
- **object** — a serviced object: name, address, coordinates, type, QR token. The center of the model.
- **checklist_template / checklist_template_item** — checklist templates. **Versioned**: editing creates a new version; executions reference a specific version — old reports never break when a template changes.
- **work_order** — a planned job: object + template (version) + date/schedule + assignee.
- **work_execution** — the fact of execution: who, when, per-item statuses, comments.
- **issue** — an incident: object, description, photos, source (worker / later — public QR), status (open → assigned → resolved → verified), assignee, due date.
- **issue_resolution** — the resolution of an issue; references either a standalone visit or a work_execution (the "closed within a planned visit" link).
- **photo** — a file in S3 + metadata: capture time, geotag, link to execution/issue, kind (before/after/defect). EXIF preserved.
- **report** — a generated report: period, objects, link to the PDF in S3.

Invariants:
- Photos are immutable (legal artifacts): no overwrites; bucket versioning / write-once policy.
- All mutations from the mobile client are idempotent: client-generated UUIDs as primary keys of created entities.

## Offline sync (the heart of the mobile app — designed before the UI)

- SQLite on the device is the worker's source of truth.
- **Mutation queue:** every action (checking an item, a photo, closing a job) goes into a local queue and is sent when the network appears. Retries with exponential backoff.
- **Idempotency:** client-generated UUIDs; re-sending never creates duplicates.
- **Conflicts:** last-write-wins — sufficient for the domain (one worker per object; overlaps are rare).
- **Photos:** uploaded directly to S3 via **presigned URLs** (the API issues a URL, the client PUTs; on interruption — retry the PUT). The API never becomes a bottleneck for gigabytes of photos.
- **Sync status in the UI is a first-class citizen:** "3 photos waiting to upload", "everything synced". The worker must trust the system.
- iOS background execution is unreliable: don't count on "it'll upload in the background eventually"; design for foreground upload + fast resume.
- Off-the-shelf options (WatermelonDB, PowerSync) were considered; for this simple model a transparent hand-rolled SQLite queue is preferred.

## Auth: details

- Admin/manager: email + password (argon2id), refresh tokens with rotation.
- Worker: the admin creates a worker → a 6-digit invite code is generated → the worker signs in with the code on their device and receives a long-lived device token. Revocation — by deactivating the worker.
- No phone numbers/emails for workers unless truly needed.

## 152-FZ and data

- Production hosting: a Russian cloud (Yandex Cloud / VK Cloud) — Russian citizens' data stays in Russia.
- Legal construction: the client (a legal entity) is the data controller for its workers; Sdano is the processor. To be fixed in the contract/terms before real sales; consult a 152-FZ lawyer.
- Photo geotags are the coordinates of a work object within a job function; addressed in the agreement during client onboarding.

## Observability (starter minimum)

- Structured logs (slog) → stdout → docker logs / journald.
- Health endpoints; external uptime pinger.
- Sentry (or self-hosted GlitchTip) for mobile and admin errors — as users appear.

## What we deliberately do NOT do at the start

- Kubernetes, microservices — one Go binary.
- Self-service tenant registration — onboarding by hand.
- Payment integration — invoices manually.
- Push notifications, complex roles, analytics — driven by paying clients' requests.
