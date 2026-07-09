# Sdano — API Specification (Slice 0–1)

Style: REST-ish JSON over HTTPS, defined code-first via huma (the OpenAPI 3.1 spec and Scalar docs at `/docs` are generated from handler types — this document describes intent; the generated spec is the contract). All endpoints are tenant-scoped through the authenticated principal; `tenant_id` never appears in URLs or bodies.

Base path: `/api/v1`.

## Conventions

- **IDs:** UUIDs. Mobile-created resources send their own `id` (client-generated, idempotent upsert on the server).
- **Errors:** RFC 7807 `application/problem+json` (huma's default). Stable machine-readable `type` slugs, e.g. `invite-code-invalid`, `work-order-not-assigned`.
- **Timestamps:** RFC 3339 with offset. Mobile sends device-clock times explicitly where the field name says so.
- **Idempotency:** all mobile POSTs are upserts keyed by client UUID. Replaying a request is always safe and returns 200 with the current state (not 409).
- **Pagination:** cursor-based, `?cursor=...&limit=...`, response carries `next_cursor`. Only where lists can grow (executions, issues, photos).
- **Auth transport:** `Authorization: Bearer <token>`. Two principal kinds: staff (admin/manager JWT + refresh) and worker (long-lived device token).

## Auth

### Staff (admin/manager)
```
POST /auth/login              { email, password } → { access_token, refresh_token, user }
POST /auth/refresh            { refresh_token }    → { access_token, refresh_token }   # rotation
POST /auth/logout             { refresh_token }    → 204
```
Access token: 15 min JWT. Refresh: opaque, rotated on every use, stored hashed.
Login and refresh reject an **archived** tenant with `401 tenant-archived` — a dead
tenant is never issued tokens (see 12-platform-ops.md); a **suspended** tenant still
authenticates (read-only staff access). Login runs a constant-time argon2id
verification on every credential miss (unknown email, inactive user), so response
latency never reveals whether an account exists at an email.

### Worker (device)
```
POST /auth/worker/claim       { invite_code, device_name? } → { device_token, worker }
```
Single-use code within its TTL; issues a long-lived device token (opaque, hashed at rest). Revocation happens server-side by deactivating the worker or the token — the next API call returns 401 and the app drops to the sign-in screen. A **deactivated** worker's invite is rejected up front as `invite-code-invalid` (the device token could never authenticate); claiming under an **archived** tenant returns `401 tenant-archived`.

## Worker-facing API (mobile)

Everything the mobile app needs for slice 1, shaped for offline-first: one bootstrap read, then write-only mutations.

### Sync bootstrap / refresh
```
GET /worker/today
```
Returns the worker's full working set for the day in one payload (the mobile app persists it into SQLite and can then operate fully offline):
```jsonc
{
  "generated_at": "...",
  "objects":   [ { "id", "name", "address", "lat", "lon", "qr_token" } ],
  "work_orders": [
    {
      "id", "object_id", "due_date", "status",
      "checklist": {                      // denormalized pinned version
        "version_id",
        "items": [ { "id", "position", "title", "requires_photo" } ]
      }
    }
  ]
}
```
Design note: denormalizing the checklist into the payload means the mobile app never joins template tables and never sees a template change mid-day — it works against exactly what it downloaded.

### Executions (idempotent upserts)
```
PUT /worker/executions/{id}
```
```jsonc
{
  "work_order_id": "...",
  "started_at": "...",            // device clock
  "finished_at": "...",           // device clock; null while in progress
  "items": [ { "id", "template_item_id", "checked", "checked_at" } ],
  "note": null
}
```
Full-state upsert: the client always sends the complete current state of the execution; the server replaces. This makes the offline queue trivial — no diffs, no ordering hazards between item-level updates. Response: 200 with the server view (including any photos it knows about).

### Photos — two-phase upload
```
POST /worker/photos/presign
  { "id", "execution_id", "kind", "content_type" }
  → { "upload_url", "s3_key", "expires_at" }

# client PUTs the bytes to upload_url (direct to S3, resumable by retrying the PUT)

POST /worker/photos/{id}/confirm
  { "taken_at", "lat", "lon" }
  → 200 { photo }
```
Confirm triggers a server-side HEAD to S3 to verify the object exists, then stamps `uploaded_at`. Both calls are idempotent: re-presigning an unconfirmed photo returns a fresh URL for the same key; re-confirming returns current state. Orphans (`uploaded_at IS NULL` older than N days) are garbage-collected.

### QR resolution
```
GET /worker/objects/by-qr/{qr_token}   → { object, today_work_order? }
```
Works offline too — the mobile app first checks its local SQLite (qr_token is in the bootstrap payload); the endpoint exists for objects outside today's route.

## Staff-facing API (admin panel)

Slice 1 is read-heavy for staff; writes are object/order management.

### Dashboard
```
GET /staff/dashboard?date=2026-07-08
→ { totals: { done, in_progress, overdue, total },
    objects: [ { object, today_status, last_activity_at, worker_name, photo_count } ] }
```
One endpoint answering "is everything okay today?" — the 3-second screen renders from a single call.

### Objects
```
GET    /staff/objects                          # list, filter by contract/active
POST   /staff/objects                          # create
PATCH  /staff/objects/{id}
GET    /staff/objects/{id}                     # card: recent executions, open issues
GET    /staff/objects/{id}/executions?cursor   # history
```

### Work orders
```
POST   /staff/work-orders                      # single or bulk create (array accepted)
GET    /staff/work-orders?date=&object_id=&status=
PATCH  /staff/work-orders/{id}                 # reassign / reschedule
```
Bulk create is how the "pre-generated schedule" works in slice 1: the admin (or a script) creates a week of orders in one call.

### Workers & invites
```
GET    /staff/workers
POST   /staff/workers                          # { display_name } → creates user + invite code
POST   /staff/workers/{id}/reinvite            # new code, old tokens revoked optionally
PATCH  /staff/workers/{id}                     # rename / deactivate
```

### Photos & evidence
```
GET /staff/executions/{id}                     # full execution: items, photos (presigned GET URLs)
GET /staff/photos/{id}/url                     # short-lived presigned GET (lightbox)
```
Reads of photo bytes also go through presigned URLs — the API serves JSON only, never streams images.

### Reports
```
POST /staff/reports        { contract_id, period_from, period_to } → 202 { report_id, status: "generating" }
GET  /staff/reports/{id}   → { status: "generating" | "ready" | "failed", download_url? }
GET  /staff/reports        # history
```
Generation is async (headless Chrome can take tens of seconds for a month of photos): 202 + polling. The admin UI shows "PDF will be ready in 1–2 minutes" and polls; download_url is a presigned GET.

## Slice 2 additions (sketched now so the shape is stable)

```
PUT  /worker/issues/{id}                        # create/update an issue (client UUID, photos via same presign flow)
GET  /worker/today                              # payload gains "open_issues" per object
PUT  /worker/issue-resolutions/{id}             # { issue_id, execution_id?, resolved_at, note }
GET  /staff/issues?status=&object_id=&cursor
PATCH /staff/issues/{id}                        # assign / change status / due date
```
The `execution_id` on a resolution is the loop link: "fixed during the planned visit."

## Cross-cutting

- **Tenant status enforcement (one middleware, not per-handler checks).** The authenticated principal's tenant status gates requests per the table in 12-platform-ops.md: `trial`/`active` — full access; `suspended` — reads and exports allowed, mutations rejected with problem type `tenant-suspended` (403), **except** outbox flushes of work performed before suspension (`device_finished_at` < suspension timestamp) — evidence of performed work is never rejected on billing grounds; `archived` — 401. Report generation for past periods remains available under `suspended`. The public `/auth/*` token-minting routes sit outside this middleware, so login, refresh, and worker-claim enforce `archived` in the auth service itself: an archived tenant gets `401 tenant-archived` instead of tokens (`suspended`/`trial`/`active` authenticate and are then gated per-request here).
- **Rate limiting:** two-tier. Pre-auth, per real client IP (resolved from `X-Forwarded-For` behind the trusted proxy): strict on `/api/v1/auth/*`, an isolated class for `/healthz`, a generous DoS ceiling elsewhere. Post-auth, per verified principal — generous for workers (photo bursts are legitimate). Over-budget requests get `429` with the `rate-limited` problem type. Budgets are tunable, not contract.
- **Versioning:** `/v1` in the path; additive changes preferred; breaking changes = new version (unlikely before the OSS pivot).
- **Clock skew:** the server never rejects device timestamps for being "in the past"; it stores both device and server times (see data model, `device_finished_at`).
- **CORS:** admin origin only; the mobile app doesn't need CORS.
- **OpenAPI artifacts:** CI publishes the generated spec; orval runs against it to regenerate `packages/api-client` — a PR fails if generated clients drift from committed ones.
