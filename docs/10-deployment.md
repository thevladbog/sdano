# Sdano — Deployment & Operations

Philosophy: **one docker-compose, any VM, boring on purpose.** The same compose file is the local dev stand, the production deployment in a Russian cloud, and the future self-hosted/OSS distribution. Operational simplicity is a feature: evenings go into the product, not into the platform.

## Topology (production, first year)

One VPS in a Russian cloud (Yandex Cloud / VK Cloud — 152-FZ data residency):

```
┌─ VPS ──────────────────────────────────────────┐
│  Caddy (:443, TLS auto via Let's Encrypt)      │
│    ├── app.sdano.app      → admin (Next.js)    │
│    ├── api.sdano.app      → api (Go)           │
│    └── api.sdano.app/docs → Scalar (via huma)  │
│  api        (Go binary, single container)      │
│  admin      (Next.js, standalone output)       │
│  postgres   (16, volume-backed)                │
└────────────────────────────────────────────────┘
   Object storage: Yandex Object Storage (managed, S3 API)
```

- **Object storage is the managed service, not self-hosted MinIO, in production.** Photos are the legal payload; a managed store's durability beats a MinIO container on the same disk as everything else. MinIO runs only in dev/CI (compose profile `dev`).
- Postgres self-hosted in the container initially (cost), with a clean migration path to the cloud's managed Postgres when revenue justifies it — the app only knows a DSN.
- Sizing to start: 2 vCPU / 4 GB / 40 GB SSD covers dozens of tenants at this workload (the API is JSON-only; photo bytes bypass it entirely via presigned URLs).

## Compose layout

```yaml
# deploy/docker-compose.yml (sketch)
services:
  caddy:    { image: caddy, ports: ["80:80","443:443"], volumes: [caddy_data, ./Caddyfile] }
  api:      { image: ghcr.io/<org>/sdano-api:${TAG}, env_file: .env, depends_on: [postgres] }
  admin:    { image: ghcr.io/<org>/sdano-admin:${TAG}, env_file: .env }
  postgres: { image: postgres:16, volumes: [pg_data], env_file: .env }
  minio:    { image: minio/minio, profiles: [dev] }   # dev/CI only
```

- All configuration via `.env` (12-factor): DSN, S3 endpoint/bucket/keys, JWT secret, base URLs. **No config file is ever baked into an image** — the OSS pivot inherits this for free.
- `TAG` pins the release; rollback = change TAG, `docker compose up -d`.
- Migrations run as a one-shot container/step before the new api starts (`migrate -path db/migrations up`); migrations are backward-compatible by policy (new code must run against schema N and N+1) so a rollback never needs a down-migration in anger.

## CI/CD (GitHub Actions)

Pipeline on main:
1. Lint + tests (Go, TS), sqlc generate check (committed code matches queries), orval drift check (committed clients match the OpenAPI spec).
2. Build images (api, admin), push to GHCR with the commit SHA tag.
3. Deploy job (manual approval initially): SSH to the VPS, update TAG in .env, `docker compose pull && up -d`, run migrations, health-check gate.

Mobile: EAS builds; OTA (expo-updates) for JS-only changes — a field bug fixed without store review. Store builds for native changes only.

## Backups (the paranoid section — this is legal evidence)

Two data classes, two strategies:

- **Postgres:** nightly `pg_dump` (custom format) → uploaded to a **separate** Object Storage bucket in a different account/project than production credentials; 30 daily + 12 monthly retention. Weekly automated restore test into a scratch container in CI ("a backup that hasn't been restored is a hope, not a backup").
- **Photos/PDFs (Object Storage):** bucket **versioning ON**, delete operations denied to the app's IAM key (the app can only PUT and GET — even a compromised server cannot destroy evidence). Lifecycle: keep all versions until the retention question from the data model (open question #4) is answered by a real client; default posture is "keep everything," photos are small money compared to their dispute value.
- `.env` (secrets) backed up manually to a password manager; documented in the runbook.

**RPO/RTO honesty:** worst case loses <24h of DB writes (last nightly dump) — but *not* the photos (already in Object Storage) and *not* worker-side data (their outbox re-syncs on reconnect: the offline-first architecture is itself a disaster-recovery mechanism). Recovery: new VPS + compose + restore dump ≈ under 2 hours, rehearsed once before the first paying client.

## Monitoring (starter kit)

- Uptime: external pinger (e.g. UptimeRobot-class) on `/healthz` (api: checks DB + S3 reachability) and the admin origin. Alerts → Telegram.
- Errors: self-hosted GlitchTip (or Sentry free tier) — api, admin, and the mobile app (the outbox `blocked` state reports here; that alert is a customer-trust incident, not a metric).
- Logs: `slog` JSON → docker logs, `docker compose logs` + `lnav` is genuinely enough at this scale; log shipping is a later problem.
- Disk-space alert on the VPS (Postgres + Caddy certs live there; photos deliberately don't).

## Security posture

- VPS: SSH keys only, non-root deploy user, ufw (80/443/22), unattended-upgrades.
- Caddy: TLS 1.2+, HSTS. API: strict CORS (admin origin). Rate limiting is app-level and two-tier: a pre-auth per-client-IP shield (strict on `/api/v1/auth/*`, an isolated class for `/healthz`, a generous ceiling elsewhere) plus a per-principal tier for authenticated traffic. The real client IP is read from `X-Forwarded-For` via one trusted proxy hop — set `TRUSTED_PROXY_COUNT=1` in the prod `.env` so Caddy's address does not become one global bucket.
- Secrets never in the repo (env only); the repo is written as-if-public from day one (pivot discipline).
- S3 IAM: production key can PUT/GET but not DELETE (see backups); presigned URLs are short-lived (15 min PUT, 5 min GET).
- Postgres not exposed publicly (compose-internal network only).

## Environments

| Env | Where | Data | Purpose |
|---|---|---|---|
| dev | laptop, compose `--profile dev` (MinIO) | fixtures/seed script | daily development |
| demo | small VPS or the prod box with a demo tenant | client #1's real bus stops (public data) + our photos | the slice-1 demo and ongoing sales demos |
| prod | RU-cloud VPS | real tenants | paying clients |

A `make seed-demo` target loads the demo tenant (objects from a CSV, a template, a week of work orders) — the "come with their data" demo trick is a one-command operation, reusable for every next prospect.

## Self-hosted story (kept warm for the OSS pivot)

Everything above already *is* the self-hosted distribution: compose file + .env.example + this document. The only pivot-day additions: a hardened `.env.example`, an install section in the README, and swapping the demo-tenant seed for a generic one. Deliberately no Helm chart, no Terraform — a contractor's IT guy (or a municipal admin) can read a compose file; that audience is the point.
