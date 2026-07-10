# Reports + Ops Implementation Plan (spec phase 6)

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Ship the last spec phase — asynchronous dispute-grade PDF reports (HTML templates → chromedp → S3), the nightly `missed` job, orphan-photo GC, `make seed-demo`, and the `sdano-ops` CLI with `tenant.suspended_at` powering the precise suspension carve-out.

**Architecture:** New `internal/report` package (aggregate queries → embedded `html/template` set → photo downscale → chromedp `PrintToPDF` against the compose `headless-shell` → S3 upload; the `report` rows ARE the queue, drained by one in-process goroutine with `FOR UPDATE SKIP LOCKED` + attempt cap). New `internal/platform` package hosts the hourly scheduler (tenant-timezone `missed` marking, orphan GC). `cmd/ops` is the operator CLI (reuses domain packages; every mutation writes `ops_audit`). `cmd/seed` loads the demo tenant. Normative: `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md` (phase 6), `docs/09-pdf-report.md`, `docs/12-platform-ops.md`, `docs/07-api-spec.md` (reports endpoints).

**Tech Stack:** Go 1.26.5, huma v2.38, pgx v5, sqlc 1.31.1, `github.com/chromedp/chromedp` (remote allocator), `golang.org/x/image/draw` (photo downscale), embed.FS templates, testcontainers-go, orval 8.

## Global Constraints

- **Reports are immutable evidence.** A generated PDF is never overwritten; regeneration = a NEW report row + new S3 key. Failed renders mark the row `failed` with `failure_reason`; partial PDFs are never uploaded. Missing/unconfirmed photos render as an explicit «Фото не загружено» placeholder — never silently skipped (docs/09).
- **Queue semantics (judgment call, refines the spec's "mark stale generating failed on startup"):** `status='generating'` is the queued/in-progress state; the worker picks the oldest by `created_at` with `FOR UPDATE SKIP LOCKED`, increments `render_attempts`, and a row reaching **3 attempts** goes `failed` («render failed after 3 attempts»). Crash mid-render ⇒ the row is simply retried on the next poll — no startup sweep can kill queued-but-unstarted rows. Poll interval 5s.
- **Report pipeline exact values (docs/09):** photos downscaled to **~1200px long edge, JPEG quality 80**, embedded as data URIs; report short ID = `SD-` + first 4 bytes of the report UUID hex, uppercase (e.g. `SD-3F8A11C2`); A4, RU strings in templates (externalized to a `strings` map in the template data — EN later); `@page` CSS pagination; photo grids 4–6 per page.
- **Suspension (docs/12 + `tenant.suspended_at`, added here):** suspended tenants CAN generate reports for past periods (`POST /staff/reports` carries `auth.SuspendedWritable()`). The worker evidence-flush carve-out becomes precise: under suspension, the execution upsert requires `device_finished_at` non-null AND `< suspended_at`; photo presign/confirm require the parent execution's `device_finished_at < suspended_at`. Otherwise 403 `tenant-suspended`. When `suspended_at` is NULL on a suspended tenant (legacy/manual), fall back to the blanket allow (never reject evidence on billing grounds).
- **`sdano-ops` (Phase A minimum, docs/12):** `tenant create --name … [--trial-days 30]` (creates tenant + first admin, prints credentials ONCE), `tenant list`, `tenant suspend <id> [--note]`, `tenant activate <id>`, `tenant set-billing <id> --billed-until YYYY-MM-DD [--plan-note …]`. Every mutating command inserts an `ops_audit` row `{action, tenant_id, detail jsonb}`. No network surface — runs against `DATABASE_URL`. `archive`/`stats`/`export-tenant` are deliberately deferred (spec scope).
- **Nightly `missed` (docs/06 decision 7) is tenant-timezone-aware:** `UPDATE work_order SET status='missed' FROM tenant t WHERE … status='scheduled' AND due_date < (now() AT TIME ZONE t.timezone)::date`. Orphan-photo GC: rows with `uploaded_at IS NULL` older than **14 days** are deleted **only after verifying the S3 object does not exist** (evidence rule); if the object exists, the row is left alone (the worker may still confirm). Scheduler ticks hourly; both jobs are idempotent.
- **Migration 0004 (additive, reversible):** `tenant.suspended_at timestamptz`, `report.created_at timestamptz NOT NULL DEFAULT now()`, `report.render_attempts int NOT NULL DEFAULT 0`, `photo.created_at timestamptz NOT NULL DEFAULT now()` (rules §5), index `report (status, created_at) WHERE status = 'generating'`. docs/06 updated same task.
- **Config:** new env `CHROME_CDP_URL` (default `http://localhost:9222`; compose api service gets `http://headless-shell:9222`), optional — not in the required-vars set; `.env.example` + compose updated in the same task that consumes it. `ObjectStore` gains `Get(ctx, key) ([]byte, error)` and `Put(ctx, key, contentType string, body []byte) error` (server-side, for report photo reads + PDF upload).
- **New deps (latest stable, pinned):** `github.com/chromedp/chromedp@latest`, `golang.org/x/image@latest`. Both justified: chromedp is the spec-mandated renderer; x/image for high-quality downscaling.
- **New slugs (document in docs/07 in the final task):** `report-not-found` (404), `invalid-period` (422, from>to or >92 days), `renderer-unavailable` — NOT used (failures are per-row `failed`); drop it. Final list: `report-not-found`, `invalid-period`.
- **Never hand-edit generated code**; each endpoint task regenerates the client; `make drift` green. huma: optional pointers need `,omitempty`. Zero lint; `slog` only; errors wrapped. Conventional commits as `Vladislav Bogatyrev <vladislav.bogatyrev@gmail.com>` with the `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer on EVERY commit.
- **Tests:** Postgres via testcontainers (podman env: `export DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')`, `export TESTCONTAINERS_RYUK_DISABLED=true`). Chrome is NOT required by unit/integration tests — the renderer is behind a `PDFRenderer` interface with a fake; the live chromedp render is exercised once in the final sweep against the compose headless-shell. Report aggregate queries get fixture tests (rules §6.3: wrong numbers in a municipal report are a trust incident).

## Consumed interfaces (verify against main before use)

- `photo.ObjectStore{PresignPut, Exists, PresignGet}` (+ this plan adds Get/Put); `photo.NewS3Store(client, bucket)`; fakes in `photo/http_test.go`.
- `workorder.Register(api, pool, store photo.ObjectStore)`; `photo.Register(api, pool, store)`; `object.Register(api, queries)`; `roster.Register(api, pool)` — wired in `app.New` (`apps/api/internal/app/app.go`).
- `workorder.TenantToday(ctx, q, tenantID) (pgtype.Date, error)`; `auth.PrincipalFrom`; `auth.SuspendedWritable()`; `auth.HashPassword(pw) (string, error)`; `roster.CreateInvite(ctx, q, tenantID, userID) (code string, expiresAt time.Time, err error)`.
- `db.New(pool)`, `WithTx`; sqlc conventions (NullUUID/pgtype/pointers); `app.Deps{Pool, S3, Checks}`; `main.go run()` owns the signal context — background goroutines hook there.
- Per-package `problem(status, slug, detail)` helpers exist; copy the 3-liner into new packages.

## Task index

1. Migration 0004 + sqlc queries (report/ops/scheduler/GC) + docs/06
2. Report data layer + HTML templates + `cmd/report-preview` (offline, fixtures)
3. Store Get/Put + photo downscale + chromedp `PDFRenderer`
4. Report worker (queue drain, attempts, upload) + main.go/config wiring
5. Report HTTP endpoints (202/poll/list) + client regen
6. Scheduler: tenant-tz `missed` + orphan-photo GC + main.go wiring
7. `sdano-ops` CLI (create/list/suspend/activate/set-billing + ops_audit)
8. Precise suspension carve-out in worker evidence handlers
9. `make seed-demo` + docs sync + final verification sweep (live PDF render)

---

### Task 1: Migration 0004 + sqlc queries + docs/06

**Files:**
- Create: `db/migrations/0004_suspension_report_queue.up.sql`, `.down.sql`
- Create: `db/queries/report.sql`, `db/queries/platform.sql`
- Modify: `docs/06-data-model.md`
- Generated (committed): `apps/api/internal/db/report.sql.go`, `platform.sql.go`

**Interfaces:**
- Produces (trust generated types): report — `InsertReport(...{TenantID uuid.UUID; ContractID uuid.NullUUID; PeriodFrom, PeriodTo pgtype.Date; GeneratedBy uuid.NullUUID}) (InsertReportRow{ID, Status, CreatedAt}, error)`; `GetReport(...{ID, TenantID})`; `ListReports(tenantID)` (LIMIT 100 DESC); `ClaimNextReport(ctx) (ClaimNextReportRow, error)` (SKIP LOCKED — worker-internal, no tenant param by design: the worker serves all tenants); `MarkReportReady(...{ID, S3Key})`; `MarkReportFailed(...{ID, FailureReason})`; report DATA queries: `ReportSummaryRows(...{TenantID; ContractID uuid.NullUUID; From, To pgtype.Date})` (per-object planned/done/missed counts), `ReportObjectExecutions(...)` (completed executions in period w/ worker name + items n/n), `ReportExecutionPhotos(executionIDs []uuid.UUID)`, `ReportMissedOrders(...)`, `GetTenantName(id)`, `GetContractName(...{ID, TenantID})`.
- platform — `MarkOverdueOrdersMissed(ctx) (int64, error)` (`:execrows`, tenant-tz aware, cross-tenant by design — the scheduler serves all tenants); `ListOrphanPhotos(olderThan pgtype.Timestamptz) ([]ListOrphanPhotosRow{ID, S3Key}, error)` (LIMIT 100); `DeletePhotoRow(id uuid.UUID)`; ops — `OpsCreateTenant(...{Name string; TrialEndsAt pgtype.Timestamptz})`, `OpsListTenants()` (name/status/billed_until/trial_ends_at/worker+object counts), `OpsSetTenantStatus(...{ID; Status; SuspendedAt pgtype.Timestamptz})`, `OpsSetBilling(...{ID; BilledUntil pgtype.Date; PlanNote *string})`, `InsertOpsAudit(...{Action string; TenantID uuid.NullUUID; Detail []byte})`, `GetTenantSuspension(id) (pgtype.Timestamptz, error)` (returns suspended_at; used by task 8).

- [ ] **Step 1: Migration.** `0004_suspension_report_queue.up.sql`:
```sql
-- Operator suspension timestamp: the precise evidence carve-out boundary
-- (docs/12 — work with device_finished_at < suspended_at is always accepted).
ALTER TABLE tenant ADD COLUMN suspended_at timestamptz;
-- rules §5: created_at on everything; report rows double as the render queue.
ALTER TABLE report ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
ALTER TABLE report ADD COLUMN render_attempts int NOT NULL DEFAULT 0;
ALTER TABLE photo ADD COLUMN created_at timestamptz NOT NULL DEFAULT now();
CREATE INDEX report_queue_idx ON report (created_at) WHERE status = 'generating';
```
`.down.sql` drops the index and three columns (reverse order, `IF EXISTS`). Verify `make dev-up && make migrate-up && make migrate-down && make migrate-up` all exit 0.

- [ ] **Step 2: `db/queries/report.sql`.**
```sql
-- name: InsertReport :one
INSERT INTO report (tenant_id, contract_id, period_from, period_to, generated_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING id, status, created_at;

-- name: GetReport :one
SELECT id, contract_id, period_from, period_to, status, failure_reason, s3_key, generated_at, created_at
FROM report WHERE id = $1 AND tenant_id = $2;

-- name: ListReports :many
SELECT id, contract_id, period_from, period_to, status, generated_at, created_at
FROM report WHERE tenant_id = $1
ORDER BY created_at DESC LIMIT 100;

-- name: ClaimNextReport :one
-- Worker-internal: drains the queue across ALL tenants (single in-process
-- worker). SKIP LOCKED lets a future second instance coexist safely.
UPDATE report SET render_attempts = render_attempts + 1
WHERE id = (
    SELECT id FROM report
    WHERE status = 'generating' AND render_attempts < 3
    ORDER BY created_at
    LIMIT 1
    FOR UPDATE SKIP LOCKED
)
RETURNING id, tenant_id, contract_id, period_from, period_to, render_attempts;

-- name: MarkReportReady :exec
UPDATE report SET status = 'ready', s3_key = $2, generated_at = now()
WHERE id = $1 AND status = 'generating';

-- name: MarkReportFailed :exec
UPDATE report SET status = 'failed', failure_reason = $2
WHERE id = $1 AND status = 'generating';

-- name: GetTenantName :one
SELECT name FROM tenant WHERE id = $1;

-- name: GetContractName :one
SELECT name, client_name FROM contract WHERE id = $1 AND tenant_id = $2;

-- name: ReportSummaryRows :many
SELECT o.id AS object_id, o.name AS object_name, o.address,
       count(wo.id) AS planned,
       count(wo.id) FILTER (WHERE wo.status = 'done') AS done,
       count(wo.id) FILTER (WHERE wo.status = 'missed') AS missed
FROM work_order wo
JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
WHERE wo.tenant_id = sqlc.arg(tenant_id)
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND (sqlc.narg(contract_id)::uuid IS NULL OR o.contract_id = sqlc.narg(contract_id))
GROUP BY o.id, o.name, o.address
ORDER BY o.address NULLS LAST, o.name;

-- name: ReportObjectExecutions :many
SELECT wo.object_id, e.id AS execution_id, wo.due_date,
       e.device_finished_at, e.finished_at, u.display_name AS worker_name,
       (SELECT count(*) FROM work_execution_item i WHERE i.execution_id = e.id AND i.checked) AS checked_items,
       (SELECT count(*) FROM work_execution_item i WHERE i.execution_id = e.id) AS total_items
FROM work_execution e
JOIN work_order wo ON wo.id = e.work_order_id AND wo.tenant_id = e.tenant_id
JOIN app_user u ON u.id = e.worker_id
WHERE e.tenant_id = sqlc.arg(tenant_id)
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND e.device_finished_at IS NOT NULL
  AND (sqlc.narg(contract_id)::uuid IS NULL
       OR EXISTS (SELECT 1 FROM object o WHERE o.id = wo.object_id AND o.contract_id = sqlc.narg(contract_id)))
ORDER BY wo.object_id, e.device_finished_at;

-- name: ReportExecutionPhotos :many
SELECT execution_id, id, kind, s3_key, taken_at, lat, lon, uploaded_at
FROM photo
WHERE tenant_id = sqlc.arg(tenant_id)
  AND execution_id = ANY(sqlc.arg(execution_ids)::uuid[])
ORDER BY execution_id, kind, id;

-- name: ReportMissedOrders :many
SELECT wo.object_id, o.name AS object_name, wo.due_date
FROM work_order wo
JOIN object o ON o.id = wo.object_id AND o.tenant_id = wo.tenant_id
WHERE wo.tenant_id = sqlc.arg(tenant_id)
  AND wo.status = 'missed'
  AND wo.due_date BETWEEN sqlc.arg(period_from) AND sqlc.arg(period_to)
  AND (sqlc.narg(contract_id)::uuid IS NULL OR o.contract_id = sqlc.narg(contract_id))
ORDER BY o.address NULLS LAST, o.name, wo.due_date;
```

- [ ] **Step 3: `db/queries/platform.sql`.**
```sql
-- name: MarkOverdueOrdersMissed :execrows
-- Tenant-timezone-aware: an order is missed once ITS tenant's local date has
-- moved past due_date. Cross-tenant by design (the scheduler serves all tenants).
UPDATE work_order wo SET status = 'missed'
FROM tenant t
WHERE t.id = wo.tenant_id
  AND wo.status = 'scheduled'
  AND wo.due_date < (now() AT TIME ZONE t.timezone)::date;

-- name: ListOrphanPhotos :many
SELECT id, s3_key FROM photo
WHERE uploaded_at IS NULL AND created_at < $1
ORDER BY created_at LIMIT 100;

-- name: DeletePhotoRow :exec
DELETE FROM photo WHERE id = $1 AND uploaded_at IS NULL;

-- name: OpsCreateTenant :one
INSERT INTO tenant (name, trial_ends_at) VALUES ($1, $2)
RETURNING id, name, status, trial_ends_at;

-- name: OpsListTenants :many
SELECT t.id, t.name, t.status, t.timezone, t.trial_ends_at, t.billed_until, t.suspended_at,
       (SELECT count(*) FROM app_user u WHERE u.tenant_id = t.id AND u.role = 'worker' AND u.is_active) AS active_workers,
       (SELECT count(*) FROM object o WHERE o.tenant_id = t.id AND o.is_active) AS active_objects
FROM tenant t ORDER BY t.name;

-- name: OpsSetTenantStatus :exec
UPDATE tenant SET status = $2, suspended_at = $3 WHERE id = $1;

-- name: OpsSetBilling :exec
UPDATE tenant SET billed_until = $2, plan_note = COALESCE(sqlc.narg(plan_note), plan_note)
WHERE id = $1;

-- name: InsertOpsAudit :exec
INSERT INTO ops_audit (action, tenant_id, detail) VALUES ($1, $2, $3);

-- name: GetTenantSuspension :one
SELECT suspended_at FROM tenant WHERE id = $1;
```

- [ ] **Step 4: Generate, build, docs, commit.** `make generate-sqlc`; `cd apps/api && go build ./... && golangci-lint run` clean. docs/06: add `suspended_at` to tenant DDL (+ rationale line "precise carve-out boundary, set by ops suspend — docs/12"), `created_at`/`render_attempts` to report, `created_at` to photo, each with "Changed on 2026-07-10 (phase 6)". Report exact generated signatures for Tasks 2–8.
```bash
git add db/migrations/ db/queries/report.sql db/queries/platform.sql apps/api/internal/db/ docs/06-data-model.md
git commit -m "feat(reports): migration 0004 and sqlc queries for reports, scheduler, ops"
```

---

### Task 2: Report data layer + HTML templates + `cmd/report-preview`

**Files:**
- Create: `apps/api/internal/report/data.go` (aggregate → `ReportData`), `apps/api/internal/report/templates.go` (embed + render), `apps/api/internal/report/templates/report.html`, `apps/api/internal/report/fixtures.go` (preview fixture)
- Create: `apps/api/cmd/report-preview/main.go`
- Test: `apps/api/internal/report/data_test.go`, `apps/api/internal/report/templates_test.go`
- Modify: `Makefile` (`report-preview` target)

**Interfaces:**
- Produces:
  - `type ReportData struct { ShortID, TenantName, ContractName, ClientName string; PeriodFrom, PeriodTo time.Time; GeneratedAt time.Time; Summary SummaryData; Objects []ObjectSection; Missed []MissedRow }`
  - `SummaryData{ObjectCount, Planned, Done, Missed int; CompletionPct int; PerObject []SummaryRow}`; `SummaryRow{Name, Address string; Planned, Done, Missed int}`; `ObjectSection{Name, Address string; Jobs []JobRow}`; `JobRow{Date, FinishedAt string; WorkerName string; CheckedItems, TotalItems int; Photos []PhotoCell}`; `PhotoCell{DataURI string; Caption string; Missing bool}` (Missing=true → «Фото не загружено» placeholder cell); `MissedRow{ObjectName, Date string}`.
  - `BuildReportData(ctx, q *db.Queries, r ClaimedReport, photoLoad PhotoLoader) (ReportData, error)` where `ClaimedReport{ID, TenantID uuid.UUID; ContractID uuid.NullUUID; PeriodFrom, PeriodTo time.Time}` and `type PhotoLoader func(ctx context.Context, s3Key string) (dataURI string, err error)` — injectable so Task 2 tests need no S3 (fake returns a 1×1 gif data URI); unconfirmed photos (uploaded_at null) become `PhotoCell{Missing: true}` WITHOUT calling the loader.
  - `RenderHTML(d ReportData) (string, error)` — executes the embedded template set; `ShortIDFor(id uuid.UUID) string` (`SD-` + hex of first 4 bytes, uppercase).
  - `PreviewFixture() ReportData` — deterministic fixture for the preview + template test.

- [ ] **Step 1: Failing tests.** `data_test.go` (testcontainers): seed tenant/contract/object×2/template/version/orders (2 done via executions with device_finished_at, 1 missed, 1 scheduled) + 2 photos (1 confirmed, 1 unconfirmed) inside the period, plus one order OUTSIDE the period and one in a different contract; call `BuildReportData` with a fake `PhotoLoader`; assert: `Summary.Planned==3`, `Done==2`, `Missed==1`, `CompletionPct==66`, per-object rows sorted, the outside-period and other-contract orders excluded, the job row shows `CheckedItems/TotalItems` correctly, confirmed photo produced a DataURI cell and the unconfirmed one produced `Missing:true` (loader called exactly once). `templates_test.go` (pure): `RenderHTML(PreviewFixture())` succeeds and the output contains: the short ID, «Отчёт о выполнении работ», the summary table with fixture numbers, «Пропущенные работы», «Фото не загружено», the signature blocks «Исполнитель»/«Представитель заказчика», and `@page` CSS. `TestShortIDFor`: uuid `3f8a11c2-...` → `SD-3F8A11C2`.

- [ ] **Step 2: RED.** `go test ./internal/report/` → undefined symbols.

- [ ] **Step 3: Implement.** `data.go`: run the five Task-1 report queries; group executions by object; format dates `02.01.2006`, times `15:04` (device time per docs/09); captions «HH:MM · lat, lon» (skip missing coords); compute `CompletionPct = done*100/planned` (0 when planned==0). `templates.go`:
```go
//go:embed templates/*.html
var templateFS embed.FS

var reportTmpl = template.Must(template.ParseFS(templateFS, "templates/*.html"))

func RenderHTML(d ReportData) (string, error) {
	var buf bytes.Buffer
	if err := reportTmpl.ExecuteTemplate(&buf, "report.html", d); err != nil {
		return "", fmt.Errorf("rendering report template: %w", err)
	}
	return buf.String(), nil
}
```
`templates/report.html` — one file with `{{define}}` blocks (cover, summary, object sections, closing), complete and lean: A4 `@page { size: A4; margin: 18mm 14mm }`, `page-break-before: always` between sections, cover with «SDANO»-stamp div (bordered uppercase text, no image asset yet), title «Отчёт о выполнении работ», tenant/contract/client/period/`{{.ShortID}}`; summary headline numbers + per-object `<table>`; object sections with job rows and a photo grid (`display:grid; grid-template-columns: repeat(2, 1fr); page-break-inside: avoid`, max 6 cells per implicit page via break rules), each cell `<img src="{{.DataURI}}">` + caption strip or the placeholder box «Фото не загружено» when `.Missing`; missed list; closing page with two signature lines and footer «{{.ShortID}} · период {{…}}». All user-visible strings RU. `fixtures.go`: 2 objects, 3 jobs, 3 photos (one Missing), 1 missed row, fixed times. `cmd/report-preview/main.go`: writes `RenderHTML(PreviewFixture())` to `report-preview.html` in CWD and prints the path. Makefile:
```makefile
report-preview:
	cd apps/api && go run ./cmd/report-preview
```

- [ ] **Step 4: GREEN + lint + commit.**
```bash
go test ./internal/report/ && golangci-lint run
git add apps/api/internal/report/ apps/api/cmd/report-preview/ Makefile
git commit -m "feat(reports): aggregate data layer, ru html templates, report-preview"
```

---

### Task 3: Store Get/Put + photo downscale + chromedp PDFRenderer

**Files:**
- Modify: `apps/api/internal/photo/store.go` (interface + S3Store gain `Get`/`Put`), `apps/api/internal/photo/http_test.go` (fakeStore stubs)
- Create: `apps/api/internal/report/image.go` (downscale → data URI), `apps/api/internal/report/renderer.go` (PDFRenderer iface + chromedp impl)
- Test: `apps/api/internal/report/image_test.go`

**Interfaces:**
- Produces: `photo.ObjectStore` gains `Get(ctx context.Context, key string) ([]byte, error)` and `Put(ctx context.Context, key, contentType string, body []byte) error`; `S3Store` impls via `GetObject`/`PutObject`. `report.DownscaleJPEG(raw []byte) (dataURI string, err error)` — decode (jpeg; fall back to image.Decode), scale longest edge to ≤1200px with `draw.CatmullRom`, re-encode JPEG q80, return `data:image/jpeg;base64,…`. `report.PDFRenderer interface { RenderPDF(ctx context.Context, html string) ([]byte, error) }`; `report.NewChromeRenderer(cdpURL string) *ChromeRenderer` — chromedp `NewRemoteAllocator(ctx, cdpURL, chromedp.NoModifyURL)`, navigates a `data:text/html;base64,…` URL, waits body ready, `page.PrintToPDF().WithPrintBackground(true).WithPaperWidth(8.27).WithPaperHeight(11.69)`; 120s timeout per render.

- [ ] **Step 1: Deps.** `cd apps/api && go get github.com/chromedp/chromedp@latest golang.org/x/image@latest` — report resolved versions.
- [ ] **Step 2: Failing image test.** `image_test.go`: build a 2400×1200 JPEG in-code (`image.NewRGBA` + `jpeg.Encode`), `DownscaleJPEG` → decode the base64 payload → assert bounds ≤1200 on the long edge and aspect preserved (600 short edge), prefix `data:image/jpeg;base64,`; a tiny 100×80 image passes through un-upscaled (bounds unchanged); garbage bytes → error.
- [ ] **Step 3: Implement** `image.go` + `renderer.go` + store methods + fake stubs (`fakeStore.Get` returns bytes from a map; `Put` records into a map). chromedp code:
```go
func (r *ChromeRenderer) RenderPDF(ctx context.Context, html string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 120*time.Second)
	defer cancel()
	allocCtx, cancelAlloc := chromedp.NewRemoteAllocator(ctx, r.cdpURL, chromedp.NoModifyURL)
	defer cancelAlloc()
	taskCtx, cancelTask := chromedp.NewContext(allocCtx)
	defer cancelTask()
	var pdf []byte
	err := chromedp.Run(taskCtx,
		chromedp.Navigate("data:text/html;base64,"+base64.StdEncoding.EncodeToString([]byte(html))),
		chromedp.WaitReady("body"),
		chromedp.ActionFunc(func(ctx context.Context) error {
			var err error
			pdf, _, err = page.PrintToPDF().WithPrintBackground(true).
				WithPaperWidth(8.27).WithPaperHeight(11.69).Do(ctx)
			return err
		}),
	)
	if err != nil {
		return nil, fmt.Errorf("chrome render: %w", err)
	}
	return pdf, nil
}
```
(import `github.com/chromedp/cdproto/page`). NOTE: `NewRemoteAllocator` wants the DevTools websocket URL; passing the `http://host:9222` base works with `chromedp.NoModifyURL` absent — verify against the chromedp docs for the installed version: if an http URL must be resolved, fetch `/json/version` → `webSocketDebuggerUrl` first; implement whichever the current API requires and note it.
- [ ] **Step 4: GREEN + lint + commit.** `go test ./internal/report/ ./internal/photo/ && golangci-lint run`.
```bash
git add apps/api/internal/photo/ apps/api/internal/report/ apps/api/go.mod apps/api/go.sum
git commit -m "feat(reports): store get/put, jpeg downscale, chromedp pdf renderer"
```

---

### Task 4: Report worker + config/main wiring

**Files:**
- Create: `apps/api/internal/report/worker.go`
- Test: `apps/api/internal/report/worker_test.go`
- Modify: `apps/api/internal/config/config.go` (+`ChromeCDPURL`), `config_test.go`, `.env.example`, `deploy/docker-compose.yml` (api env `CHROME_CDP_URL: http://headless-shell:9222`), `apps/api/cmd/api/main.go` (start the worker)

**Interfaces:**
- Produces: `report.NewWorker(pool *pgxpool.Pool, store photo.ObjectStore, renderer PDFRenderer) *Worker`; `(*Worker).Run(ctx context.Context)` — loop: `ClaimNextReport`; none → sleep 5s; claimed → `BuildReportData` (PhotoLoader = store.Get→DownscaleJPEG) → `RenderHTML` → `renderer.RenderPDF` → `store.Put("tenants/{tenant}/reports/{id}.pdf", "application/pdf", pdf)` → `MarkReportReady`; any error → slog + `MarkReportFailed` ONLY when `render_attempts >= 3` (else leave generating for retry). Exits on ctx.Done. `config.Config.ChromeCDPURL string` (env `CHROME_CDP_URL`, default `http://localhost:9222`, not required).

- [ ] **Step 1: Failing worker test** (testcontainers, fake renderer + fake store): seed a minimal renderable report (tenant/object/order/execution, InsertReport). Fake renderer returns `[]byte("%PDF-fake")`. Run the worker loop body once (export a `(*Worker).tick(ctx) bool` returning whether it processed a row — Run wraps tick+sleep; test calls tick directly): assert row → `ready`, s3_key set, fake store received the PDF under the right key. Second scenario: renderer fails 3× → after third tick the row is `failed` with the attempts reason (call tick 3 times; assert render_attempts==3 and status failed; also assert the row was retried, i.e. ticks 1–2 leave it `generating`). Third: tick with empty queue returns false.
- [ ] **Step 2: RED**, **Step 3: implement** `worker.go` (tick + Run; failure branch: after a render error, re-read the claimed row's attempts — the claim already incremented — and `MarkReportFailed` if `Attempts >= 3`, message `fmt.Sprintf("render failed after %d attempts: %v", attempts, err)` truncated to 500 chars), **Step 4: GREEN**.
- [ ] **Step 5: Wiring.** config: `ChromeCDPURL: withDefault(getenv("CHROME_CDP_URL"), "http://localhost:9222")` + test. `.env.example`: `CHROME_CDP_URL=http://localhost:9222` under a `# --- Reports ---` header. compose api env: `CHROME_CDP_URL: http://headless-shell:9222`. main.go `run()`: after building deps, `store := photo.NewS3Store(s3c, cfg.S3Bucket)` (reuse for app wiring if app.New builds its own — check: app.New constructs the store internally from deps.S3; keep that, construct a second store instance here for the worker — cheap struct) then:
```go
	reportWorker := report.NewWorker(pool, store, report.NewChromeRenderer(cfg.ChromeCDPURL))
	go reportWorker.Run(ctx)
```
(ctx is the signal context — worker stops on shutdown.)
- [ ] **Step 6: Lint + full test + commit.**
```bash
git add apps/api/internal/report/ apps/api/internal/config/ apps/api/cmd/api/main.go .env.example deploy/docker-compose.yml
git commit -m "feat(reports): queue-draining render worker wired into the api process"
```

---

### Task 5: Report HTTP endpoints + client regen

**Files:**
- Create: `apps/api/internal/report/http.go`
- Test: `apps/api/internal/report/http_test.go`
- Modify: `apps/api/internal/app/app.go` (`report.Register(api, pool, store)`)
- Generated (committed): `packages/api-client/*`

**Interfaces:**
- Produces ops: `createStaffReport` POST `/api/v1/staff/reports` (body `{contract_id?: uuid, period_from: "YYYY-MM-DD", period_to: "YYYY-MM-DD"}` → **202** `{report_id, status:"generating"}`; `Metadata: auth.SuspendedWritable()` — suspended tenants may generate past-period reports per docs/12); `getStaffReport` GET `/api/v1/staff/reports/{id}` → `{id, status, period_from, period_to, failure_reason?, download_url?, url_expires_at?}` (download_url = `store.PresignGet` when ready); `listStaffReports` GET `/api/v1/staff/reports`. Validation: both dates parse (`invalid-date` 422), `from <= to` and span ≤ 92 days (`invalid-period` 422), contract (when present) belongs to tenant via `GetContractName` → else `invalid-reference` 422. Unknown report → 404 `report-not-found`. `report.Register(api huma.API, pool *pgxpool.Pool, store photo.ObjectStore)`.

- [ ] **Step 1: Failing test** (testcontainers + fake store, humatest with real auth middleware like photo tests): admin creates a report → 202 + id; GET → `status:"generating"`, no download_url; flip the row via SQL (`status='ready', s3_key='tenants/x/reports/y.pdf'`) → GET → download_url present (fake PresignGet URL) + expires; list returns it; `period_from > period_to` → 422 `invalid-period`; 93-day span → 422; foreign contract_id → 422 `invalid-reference`; unknown id → 404 `report-not-found`; worker-role token on POST → 403 (path gate).
- [ ] **Step 2: RED → implement → GREEN.** Standard handler shapes (per-package `problem` helper; pointers + omitempty; `DefaultStatus: 202` via `http.StatusAccepted`).
- [ ] **Step 3: Wire + regen + commit.** `report.Register(api, deps.Pool, store)` in app.go (reuse the store local built for photo/workorder).
```bash
cd .. && make generate-client && npm run typecheck -w packages/api-client && git add packages/api-client && make drift
git add apps/api/internal/report/ apps/api/internal/app/app.go packages/api-client
git commit -m "feat(reports): async report endpoints with 202 + polling"
```

---

### Task 6: Scheduler — tenant-tz missed + orphan GC

**Files:**
- Create: `apps/api/internal/platform/scheduler.go`
- Test: `apps/api/internal/platform/scheduler_test.go`
- Modify: `apps/api/cmd/api/main.go` (start scheduler)

**Interfaces:**
- Produces: `platform.NewScheduler(pool *pgxpool.Pool, store photo.ObjectStore) *Scheduler`; `(*Scheduler).Run(ctx)` (hourly ticker + immediate first run); exported-for-test `(*Scheduler).RunOnce(ctx) error` executing both jobs: (1) `MarkOverdueOrdersMissed` (log count when >0); (2) GC: `ListOrphanPhotos(now-14d)` → for each, `store.Exists(key)`; **absent** → `DeletePhotoRow`; **present** → skip + `slog.Warn("orphan photo has S3 object, leaving row", …)` (evidence rule — never delete a row whose bytes exist).

- [ ] **Step 1: Failing test** (testcontainers + fake store): seed (a) tenant tz `Pacific/Kiritimati` (UTC+14 — yesterday's UTC date is already "past" there) with a `scheduled` order `due_date = current_date - 1` and another `due_date = current_date + 1`; (b) three photos: orphan-old-absent (created_at 20d ago via SQL override, uploaded_at NULL, key NOT in fake store), orphan-old-PRESENT (key IS in fake store), fresh orphan (created_at now). `RunOnce`: assert past order → `missed`, future order still `scheduled`; absent-orphan row deleted; present-orphan row kept; fresh orphan kept.
- [ ] **Step 2: RED → implement → GREEN.** Straightforward; wrap errors, never abort the loop on one photo's failure (log + continue).
- [ ] **Step 3: Wire in main.go** after the report worker: `go platform.NewScheduler(pool, store).Run(ctx)`. Lint + commit.
```bash
git add apps/api/internal/platform/ apps/api/cmd/api/main.go
git commit -m "feat(platform): hourly scheduler for tenant-tz missed marking and orphan photo gc"
```

---

### Task 7: `sdano-ops` CLI

**Files:**
- Create: `apps/api/cmd/ops/main.go`
- Test: `apps/api/internal/platform/ops_test.go` + Create: `apps/api/internal/platform/ops.go` (logic lives in the package; main.go is flag-parsing only)

**Interfaces:**
- Produces (in `internal/platform`, testable; `cmd/ops` is a thin shell):
  - `OpsCreateTenant(ctx, pool, name string, trialDays int) (CreateTenantResult{TenantID uuid.UUID; AdminEmail, AdminPassword string}, error)` — creates tenant (+trial_ends_at), first admin `admin@<slug>.sdano.local` (slug = lowercased latin-ish from name, fallback tenant id prefix) with a 24-char crypto-rand password hashed via `auth.HashPassword`; audits `tenant.create`.
  - `OpsListTenants(ctx, pool) ([]db.OpsListTenantsRow, error)` (pass-through).
  - `OpsSuspend(ctx, pool, tenantID uuid.UUID, note string) error` — status `suspended` + `suspended_at=now()`; audit `tenant.suspend` with `{note}`.
  - `OpsActivate(ctx, pool, tenantID uuid.UUID) error` — status `active`, suspended_at NULL; audit `tenant.activate`.
  - `OpsSetBilling(ctx, pool, tenantID uuid.UUID, billedUntil time.Time, planNote string) error` — audit `tenant.set-billing` with `{billed_until, plan_note}`.
  - Each mutator runs its writes + audit in ONE transaction.
- `cmd/ops/main.go`: subcommands `tenant create|list|suspend|activate|set-billing` via stdlib `flag` (`flag.NewFlagSet` per subcommand), reads `DATABASE_URL` from env, prints human tables/credentials, exits 1 with usage on bad args. Uses ONLY `internal/platform` functions.

- [ ] **Step 1: Failing tests** (testcontainers): `OpsCreateTenant` → tenant exists (status trial, trial_ends_at ≈ now+30d), admin exists with role admin + working password (`auth.VerifyPassword`), audit row `tenant.create` present; `OpsSuspend` → status suspended + suspended_at set + audit; `OpsActivate` → active + suspended_at NULL + audit; `OpsSetBilling` → billed_until + plan_note + audit; audit `detail` is valid JSON.
- [ ] **Step 2: RED → implement → GREEN.** Password: 24 chars from `crypto/rand` over a 62-char alphabet. Slug: keep `[a-z0-9]` of the lowered name, else first 8 of tenant id.
- [ ] **Step 3: `cmd/ops` shell** + manual smoke against dev DB:
```bash
cd apps/api && DATABASE_URL="postgres://sdano:sdano-dev-password@localhost:5432/sdano?sslmode=disable" \
  go run ./cmd/ops tenant create --name "ЧистоГрад" --trial-days 30
DATABASE_URL=... go run ./cmd/ops tenant list
```
Expected: credentials printed once; list shows the tenant with counts.
- [ ] **Step 4: Lint + commit.**
```bash
git add apps/api/cmd/ops/ apps/api/internal/platform/
git commit -m "feat(ops): sdano-ops cli — tenant create/list/suspend/activate/set-billing with audit"
```

---

### Task 8: Precise suspension carve-out in worker evidence handlers

**Files:**
- Modify: `apps/api/internal/workorder/execution.go` + `http.go` (upsert gate), `apps/api/internal/photo/http.go` (presign + confirm gate)
- Test: extend `apps/api/internal/workorder/http_test.go`, `apps/api/internal/photo/http_test.go`
- Modify: `docs/07-api-spec.md`, `docs/08-offline-sync.md` (one-line status note each)

**Interfaces:**
- Consumes: `GetTenantSuspension(id) (pgtype.Timestamptz, error)` (Task 1); the auth middleware already lets these three ops through under suspension (`SuspendedWritable`).
- Produces behavior (docs/12 made precise): when the tenant is suspended AND `suspended_at` is set — execution upsert requires `in.DeviceFinishedAt != nil && in.DeviceFinishedAt.Before(suspendedAt)` else 403 `tenant-suspended`; photo presign/confirm require the parent execution's stored `device_finished_at` valid and `< suspended_at` else 403 `tenant-suspended`. When `suspended_at` IS NULL (legacy suspension without the ops CLI) — blanket allow (evidence is never hostage). Non-suspended tenants: zero behavior change and at most one extra indexed query per evidence call.
- Implementation shape: a tiny helper in `workorder` (exported for photo? no — copy the 5-liner per package, matching the `problem` convention): load `GetTenantStatus` is already middleware's job; here call `GetTenantSuspension` ONLY when needed. To know "suspended" without re-querying status: query status+suspended_at together — reuse `GetTenantSuspension` but ALSO needs status; simplest: one new combined query in Task 1's platform.sql? It's already written as suspended_at-only. Use two lookups? No: `GetTenantSuspension` returns suspended_at; treat `suspended_at.Valid` as "the precise gate applies" and additionally fetch status via the existing `GetTenantStatus` ONLY if suspended_at is valid (suspended_at set but status re-activated → activate clears it, so valid suspended_at ⇒ suspended; OpsActivate nulls it — rely on that invariant, documented in ops.go).
- So: `if susp, _ := q.GetTenantSuspension(ctx, tenantID); susp.Valid { …gate… }` — one query, no status check needed (invariant: only suspend sets it, activate clears it).

- [ ] **Step 1: Failing tests.** workorder: seed fixture; SQL `UPDATE tenant SET status='suspended', suspended_at = now()`; upsert with `device_finished_at` = now-1h (pre-suspension… careful: suspended_at=now, device −1h < now ⇒ allowed) → 200; upsert with device_finished_at = now+1h → 403 `tenant-suspended`; upsert with device_finished_at nil (in-progress snapshot) → 403; suspended WITHOUT suspended_at (`suspended_at=NULL`) + nil device_finished_at → 200 (blanket fallback). photo: execution with stored `device_finished_at` pre-suspension → presign 200 + confirm 200; execution finished AFTER suspended_at → presign 403; tenant not suspended → all unchanged (existing tests).
- [ ] **Step 2: RED → implement → GREEN.** Gate placement: in the execution HTTP handler before calling `UpsertExecution` (needs only the input + one query); in photo handlers right after the existing execution-ownership load (it already has the execution row — extend `GetExecutionForWorker` to also return `device_finished_at`? That query returns ID/TenantID/WorkerID; modifying its SELECT is additive — add `device_finished_at`, regenerate, adapt the two existing call sites).
- [ ] **Step 3: Docs.** docs/07 cross-cutting suspension bullet: replace "except outbox flushes of work performed before suspension (`device_finished_at` < suspension timestamp)" wording with the now-implemented precise rule + the NULL-suspended_at fallback sentence. docs/08 failure-taxonomy row for 403 tenant-suspended: note the boundary is `tenant.suspended_at` set by the ops CLI.
- [ ] **Step 4: Lint + full test + regen (GetExecutionForWorker changed) + commit.**
```bash
cd .. && make generate-sqlc && cd apps/api && go build ./... && go test ./... && golangci-lint run
cd .. && make generate-client && git add packages/api-client apps/api/internal/db && make drift
git add db/queries/worker.sql apps/api/internal/ docs/07-api-spec.md docs/08-offline-sync.md packages/api-client
git commit -m "feat(worker): precise pre-suspension evidence carve-out via tenant.suspended_at"
```

---

### Task 9: `make seed-demo` + docs sync + final verification sweep

**Files:**
- Create: `apps/api/cmd/seed/main.go`, `db/seed/objects.csv`
- Modify: `Makefile` (`seed-demo`), `docs/07-api-spec.md` (report slugs), `README.md` only if a listed command changed (verify — `make seed-demo` is already promised there)

**Interfaces:**
- Consumes: `platform.OpsCreateTenant` (Task 7 — the seed reuses it for tenant+admin), `roster.CreateInvite`, `db` queries.

- [ ] **Step 1: `db/seed/objects.csv`** — 10 rows `name,address,lat,lon` of plausible RU bus stops (invent consistent city data, e.g. «Остановка „Улица Ленина, 45“»,«ул. Ленина, 45»,56.8519,60.6122 …).
- [ ] **Step 2: `cmd/seed/main.go`.** Reads `DATABASE_URL`; aborts (exit 1, message) if a tenant named «Демо — ЧистоГрад» already exists (idempotence guard); else: `OpsCreateTenant(ctx, pool, "Демо — ЧистоГрад", 30)` + `UPDATE tenant SET timezone='Europe/Moscow'`; contract «Контракт с администрацией города»; 10 objects from the CSV (each with `qr_token = "DEMO-"+n`); checklist template «Уборка остановки» v1 with 4 items («Собрать мусор», «Вымыть павильон», «Удалить граффити/объявления», «Фото после уборки» requires_photo=true); 2 workers («Алексей, бригада 1», «Сергей, бригада 2») with invites via `roster.CreateInvite`; a week of work orders (10 objects × 7 days starting today, alternating assignees). Prints: admin email+password, both invite codes, counts. All in one transaction where practical (tenant creation via OpsCreateTenant commits itself; the rest in a second tx — acceptable; the idempotence guard protects reruns).
- [ ] **Step 3: Makefile.**
```makefile
seed-demo:
	cd apps/api && DATABASE_URL="$(DATABASE_URL)" go run ./cmd/seed
```
Smoke: `make dev-up && make migrate-up && make seed-demo` → credentials printed; rerun → friendly abort.
- [ ] **Step 4: docs/07** — add `report-not-found` (404), `invalid-period` (422) to the slug list; reports section: confirm the shipped shape (202/poll/list, SuspendedWritable note). Commit docs separately: `docs(reports): report slugs and endpoint notes`.
- [ ] **Step 5: Clean-room sweep.** Podman env; repo root: `make dev-down && make dev-up && make migrate-up && make seed-demo` then `make lint && make test && make drift` — all exit 0 (seed-demo prints credentials; note them for step 6).
- [ ] **Step 6: LIVE end-to-end report** (the money slide, against compose incl. headless-shell): rebuild api (`docker compose -f deploy/docker-compose.yml --env-file .env up -d api --build`); login as the seeded demo admin; claim a worker invite → device token; complete one execution (PUT with device_finished_at now) — photos optional (placeholder path also proves docs/09 honesty); `POST /staff/reports {period_from: <today-7d>, period_to: <today>}` → 202; poll `GET /staff/reports/{id}` until `ready` (≤2 min) → fetch `download_url` → `curl -o /tmp/demo-report.pdf` → assert `file /tmp/demo-report.pdf` says PDF and size > 10 KB. If it lands `failed`, report the failure_reason verbatim and STOP (controller decides). Record every status code.
- [ ] **Step 7: Tree/history check** (`git status --short` empty; conventional log; trailers present) and report completion: phase 6 done — the backend spec is fully implemented.

## Plan Self-Review (done at write time)

- **Spec phase-6 coverage:** report pipeline (T1–T5, per docs/09: structure, downscale 1200px/q80, immutability, failed-with-reason, placeholder honesty, report-preview), nightly missed (T6, tenant-tz), orphan GC (T6, 14d + exists-check), seed-demo (T9), sdano-ops Phase-A minimum + ops_audit + suspended_at (T7), precise carve-out (T8), docs/06/07/08 sync (T1/T8/T9). Deliberately deferred per spec: ops `archive`/`stats`/`export-tenant`, EN template, XLSX companion, static map thumbnails.
- **Judgment calls recorded:** poll-retry queue with attempt cap 3 supersedes the spec's "mark stale generating failed on startup" (crash-safe, no lost queue rows); `suspended_at.Valid ⇒ suspended` invariant (only ops suspend sets it, activate clears it) avoids a second status query per evidence call; NULL-suspended_at fallback = blanket allow; report span cap 92 days.
- **Type consistency:** `ClaimedReport`, `ReportData`/section structs, `PhotoLoader`, `PDFRenderer`, `NewWorker(pool, store, renderer)`, `(*Worker).tick`, `NewScheduler(pool, store)`, `RunOnce`, `Ops*` signatures, `ObjectStore{…,Get,Put}` used consistently across tasks; `report.Register(api, pool, store)` matches the photo/workorder registration convention.
