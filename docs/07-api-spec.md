# Sdano — API Specification (Slice 0–1)

Style: REST-ish JSON over HTTPS, defined code-first via huma (the OpenAPI 3.1 spec and Scalar docs at `/docs` are generated from handler types — this document describes intent; the generated spec is the contract). All endpoints are tenant-scoped through the authenticated principal; `tenant_id` never appears in URLs or bodies.

Base path: `/api/v1`.

## Conventions

- **IDs:** UUIDs. Mobile-created resources send their own `id` (client-generated, idempotent upsert on the server).
- **Errors:** RFC 7807 `application/problem+json` (huma's default). Stable machine-readable `type` slugs, including: `invite-code-invalid`, `tenant-archived`, `tenant-suspended`, `rate-limited`, `work-order-not-assigned` (403), `execution-id-conflict` (409), `execution-item-conflict` (409), `qr-token-taken` (409), `invalid-checklist-item` (422), `execution-not-found` (404), `photo-not-found` (404), `photo-not-uploaded` (409), `photo-id-conflict` (409), `photo-already-uploaded` (409), `unsupported-content-type` (422), `qr-not-found` (404), `object-not-found` (404), `work-order-not-found` (404), `worker-not-found` (404), `report-not-found` (404), `invalid-cursor` (422), `invalid-reference` (422), `invalid-date` (422), `invalid-period` (422), `invalid-status` (422), `invalid-active` (422), `invalid-uuid` (422), `invalid-order-batch` (422), `report-queue-full` (429).
- **Timestamps:** RFC 3339 with offset. Mobile sends device-clock times explicitly where the field name says so.
- **Idempotency:** all mobile POSTs are upserts keyed by client UUID. Replaying a request is always safe and returns 200 with the current state (not 409).
- **Pagination:** cursor-based, `?cursor=...&limit=...`, response carries `next_cursor`. Only where lists can grow (executions, issues, photos).
- **Auth transport:** `Authorization: Bearer <token>`. Two principal kinds: staff (admin/manager JWT + refresh) and worker (long-lived device token).
- **Optional bodies:** where every body field is optional (`PATCH /staff/objects/{id}`, `PATCH /staff/work-orders/{id}`, `PATCH /staff/workers/{id}`, `POST /staff/workers/{id}/reinvite`), the body itself is optional — a request without one means "no changes / defaults" and returns current state.

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
  "device_finished_at": "...",    // device clock at completion; null while in progress
  "items": [ { "id", "template_item_id", "checked", "checked_at" } ],
  "note": null
}
```
Full-state upsert: the client always sends the complete current state of the execution; the server replaces. This makes the offline queue trivial — no diffs, no ordering hazards between item-level updates. Response: 200 with the server view (including any photos it knows about). The server stamps `finished_at` (server receipt time) once when `device_finished_at` first appears; both are kept (docs/06 decision 2).

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
`totals.overdue` counts scheduled/in-progress orders whose `due_date` precedes tenant-local today; the hourly background job converts the scheduled ones to `missed`.

### Objects
```
GET    /staff/objects?active=&contract_id=     # list; unfiltered = every object (active + inactive)
POST   /staff/objects                          # create
PATCH  /staff/objects/{id}
GET    /staff/objects/{id}                     # card: recent executions, open issues
GET    /staff/objects/{id}/executions?cursor   # history
```
Object payloads carry `contract_id` (nullable) on every read — anything settable on an object is also readable back.

### Work orders
```
POST   /staff/work-orders                      # single or bulk create (array accepted)
GET    /staff/work-orders?date=&object_id=&status=
PATCH  /staff/work-orders/{id}                 # reassign / reschedule
```
Bulk create is how the "pre-generated schedule" works in slice 1: the admin (or a script) creates a week of orders in one call.
Every referenced id is validated against the tenant, and the whole batch fails atomically on the first problem (`422 invalid-reference`): unknown or cross-tenant object/version/assignee ids, inactive assignees, and **deactivated objects** — a deactivated object can't take new orders, since the assignee would see the order while the object's QR no longer resolves. A literal JSON `null` body (which satisfies the array schema but bypasses `minItems`) is rejected as `422 invalid-order-batch`.
The list returns at most 500 orders; narrow with `date`/`object_id`/`assignee_id`/`status` filters (cursor pagination will come if a real client needs it).

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
POST /staff/reports        { contract_id?, period_from, period_to } → 202 { report_id, status: "generating" }
GET  /staff/reports/{id}   → { id, contract_id?, status, period_from, period_to, created_at,
                                failure_reason?, download_url?, url_expires_at? }
GET  /staff/reports        → { reports: [ same shape as above, minus failure_reason/download_url/url_expires_at ] }
```
`period_from`/`period_to` are `YYYY-MM-DD`; `contract_id` is optional (a report can cover every object). Generation is async — the POST only inserts the report row (the render queue itself, docs/06/09) and returns immediately; the render worker picks it up and can take tens of seconds for a month of photos. The admin UI shows "PDF will be ready in 1–2 minutes" and polls `GET .../{id}`. `download_url` is a 5-minute presigned GET, present only once `status` is `ready` **and** the row has an `s3_key` — never an empty string while generating or after a failure. `failure_reason` is present only when `status` is `failed`. The list endpoint never presigns per row (it stays a single cheap query even for a long history) — fetch the single-report GET to get a fresh `download_url`.

Validation: either date failing to parse as `YYYY-MM-DD` → `422 invalid-date`; `period_from` after `period_to`, or a span over 92 days → `422 invalid-period`; a `contract_id` that doesn't belong to the caller's tenant → `422 invalid-reference`; an unknown report id → `404 report-not-found`. POST carries the suspended-tenant write exception (see Cross-cutting below): a suspended tenant may still generate reports for past periods.

Queue depth is capped per tenant: a tenant with 5 reports already in `generating` gets `429 report-queue-full` until the render worker drains one. The worker is a single FIFO consumer across all tenants, so the cap keeps one tenant's enqueue loop from starving everyone else's reports.

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

- **Tenant status enforcement (one middleware, not per-handler checks).** The authenticated principal's tenant status gates requests per the table in 12-platform-ops.md: `trial`/`active` — full access; `suspended` — reads and exports allowed, mutations rejected with problem type `tenant-suspended` (403), **except** the worker evidence endpoints (execution upsert, photo presign, photo confirm), which apply a precise in-handler gate on top of the coarse middleware carve-out: when `tenant.suspended_at` is set, the execution upsert requires `device_finished_at` to be present and strictly before `suspended_at`, and photo presign/confirm require the parent execution's *stored* `device_finished_at` to be valid and strictly before `suspended_at` — new post-suspension work is rejected `403 tenant-suspended` even though the tenant-status gate itself lets the request through. If `suspended_at` is NULL (a legacy/manual suspension predating the `sdano-ops` CLI), this precise check is skipped and the blanket carve-out applies: all worker evidence writes remain allowed, since evidence of performed work is never rejected on billing grounds. `archived` — 401. Report generation for past periods remains available under `suspended`. The public `/auth/*` token-minting routes sit outside this middleware, so login, refresh, and worker-claim enforce `archived` in the auth service itself: an archived tenant gets `401 tenant-archived` instead of tokens (`suspended`/`trial`/`active` authenticate and are then gated per-request here).
- **Rate limiting:** two-tier. Pre-auth, per real client IP (resolved from `X-Forwarded-For` behind the trusted proxy): strict on `/api/v1/auth/*`, an isolated class for `/healthz`, a generous DoS ceiling elsewhere. Post-auth, per verified principal — generous for workers (photo bursts are legitimate). Over-budget requests get `429` with the `rate-limited` problem type. Budgets are tunable, not contract.
- **Versioning:** `/v1` in the path; additive changes preferred; breaking changes = new version (unlikely before the OSS pivot).
- **Clock skew:** the server never rejects device timestamps for being "in the past"; it stores both device and server times (see data model, `device_finished_at`).
- **"Today" is tenant-local.** "today" for `/worker/today` and QR order resolution is computed in `tenant.timezone` (IANA, default UTC; set by the operator). Staff endpoints take explicit `?date=` filters. A deactivated object's QR token no longer resolves (404 `qr-not-found`).
- **CORS:** admin origin only; the mobile app doesn't need CORS.
- **OpenAPI artifacts:** CI publishes the generated spec; orval runs against it to regenerate `packages/api-client` — a PR fails if generated clients drift from committed ones.
