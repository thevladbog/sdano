# Sdano

> Everything that happens to an object is recorded and provable.

**Sdano** is a photo-evidence and reporting platform for field service contractors — the companies that clean bus stops, maintain building entrances, mow municipal grass, and service playgrounds. Their clients (municipalities, property owners) pay for proven work: no evidence, no payment. Today that evidence lives in WhatsApp chats; assembling a monthly report means two evenings of digging through photos, and disputes are lost because proof can't be found.

Sdano replaces that with a dead-simple loop: a field worker completes a checklist and takes photos that already know **which object, when, and where** — even with zero connectivity. The owner watches live completion across all objects and generates a client-ready, dispute-grade PDF report with one click.

## Why it's different

- **Offline-first for real.** Built for cheap Androids, basements, and city outskirts. A full shift works with no signal; a replayable outbox syncs everything when the network returns, and the worker always sees an honest sync status. No sync framework, no CRDTs — [a boring, provably-sufficient design](docs/08-offline-sync.md).
- **The report is the product.** Not a dashboard export — a document a contractor proudly sends to a city administration: summary page, per-object photo evidence with timestamps and coordinates, explicit gaps (honesty is credibility), signature blocks. [Spec](docs/09-pdf-report.md).
- **Two loops, linked.** Planned work (schedule → checklist → proof) and issues (found → assigned → fixed). The link classic ticketing systems miss: an issue gets closed *during* a planned visit — because "I'll be there tomorrow anyway" is how contractors actually work.
- **QR codes on physical objects.** The worker scans a sticker on the bus stop → the right checklist opens, presence proven. Later: a public "Something wrong? Report it" QR for residents — no app install, the object is baked into the code.
- **Self-hosted by design.** One `docker-compose up` deploys everything — a Russian cloud for data-residency law (152-FZ), the client's own server, or your homelab. Evidence photos are immutable; the app's storage credentials can't even delete them.
- **PII minimalism.** A worker is a display name and an invite code. No emails, no phone numbers, nothing to leak.

## Architecture at a glance

```
 Mobile (Expo/RN, SQLite outbox)          Admin (Next.js)
        │  presigned PUT ──────► S3 ◄──── presigned GET │
        ▼                                               ▼
      API (Go: chi + huma → OpenAPI 3.1 + Scalar docs)
        │
   PostgreSQL (sqlc + pgx, versioned checklist templates)
        │
   Report worker (HTML templates → headless Chrome → PDF)
```

One type pipeline end to end: **SQL → sqlc → Go → huma → OpenAPI → orval → TypeScript.** Type drift anywhere fails CI.

**Stack:** Go, PostgreSQL, Next.js, Expo/React Native, Nx monorepo, S3-compatible storage (MinIO in dev, managed object storage in prod), Docker Compose. No Kubernetes, no microservices, no ORM — [rationale for every choice](docs/02-architecture.md).

## Repository layout

```
apps/
  api/        Go backend (chi + huma)
  admin/      Next.js admin panel
  mobile/     Expo app for field workers
packages/
  api-client/ orval-generated TS clients (from the OpenAPI spec)
  shared/     shared utils and schemas
db/
  migrations/ plain SQL up/down
  queries/    .sql sources for sqlc
deploy/       docker-compose, Caddyfile
docs/         all project documentation (see below)
```

## Quick start (development)

```bash
git clone <repo> && cd sdano
cp .env.example .env
docker compose --profile dev up -d     # postgres + minio + api + admin
make migrate seed-demo                  # schema + a demo tenant with sample objects
```

- Admin panel: http://localhost:3000 (demo credentials in `.env.example`)
- API docs (Scalar): http://localhost:8080/docs
- Mobile: `cd apps/mobile && npx expo start` — sign in with the demo invite code printed by `seed-demo`

## Documentation

| Doc | What it covers |
|---|---|
| [01 — Concept](docs/01-concept.md) | Problem, segment, positioning, business model, pivot criteria |
| [02 — Architecture](docs/02-architecture.md) | Principles, stack rationale, monorepo, auth, data residency |
| [03 — Roadmap](docs/03-roadmap.md) | Vertical slices 0–4, milestones, checkpoints |
| [04 — Design brief](docs/04-design-brief.md) | The story, personas, key scenarios, design principles |
| [05 — Design review](docs/05-design-review.md) | Feedback on design concept v1 |
| [06 — Data model](docs/06-data-model.md) | Full schema DDL, invariants, design decisions |
| [07 — API spec](docs/07-api-spec.md) | Endpoints, idempotency, photo two-phase upload |
| [08 — Offline sync](docs/08-offline-sync.md) | The outbox design, failure taxonomy, sync UX contract |
| [09 — PDF report](docs/09-pdf-report.md) | Report structure, rendering pipeline, immutability |
| [10 — Deployment](docs/10-deployment.md) | Compose topology, CI/CD, backups, security posture |

## Status

Early development — building toward a live demo with a real cleaning contractor (see [roadmap](docs/03-roadmap.md)). Not accepting external contributions yet; the project is developed in the open and written up as it goes.

## License

Proprietary for now. The licensing decision (AGPL vs open-core) is deliberately deferred — see the [pivot criteria](docs/01-concept.md#strategy-and-pivot-criteria) for the honest plan.

---

*Sdano — from Russian «сдано»: "delivered." The word a field worker says twenty times a day.*
