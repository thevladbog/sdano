# Worker API Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Build the offline-first worker-facing API — one bootstrap read (`GET /worker/today`), the idempotent full-state execution upsert (`PUT /worker/executions/{id}`), two-phase photo upload (presign/confirm), and QR object resolution — on top of the merged auth middleware chain.

**Architecture:** New `workorder` and `photo` domain packages under `apps/api/internal/`, plus a worker QR route in the existing `object` package. All worker routes sit under `/api/v1/worker/*` (role gate: worker; established in auth). Mobile-created rows (`work_execution`, `work_execution_item`, `photo`) use client-generated UUID primary keys; every server write is a full-state idempotent upsert so the mobile outbox can replay any request any number of times and converge. The three evidence-flush mutations opt into `auth.SuspendedWritable()` so a suspended tenant's pre-suspension work still uploads. Spec: `docs/superpowers/specs/2026-07-09-backend-slice-0-1-design.md` (phase 4); contracts in `docs/07-api-spec.md`, `docs/08-offline-sync.md`; schema in `docs/06-data-model.md`.

**Tech Stack:** Go 1.26.5, huma v2.38 + chi v5, pgx v5, sqlc 1.31.1, aws-sdk-go-v2 (S3 presign + HeadObject), testcontainers-go, orval 8.

## Global Constraints

- **Idempotency is the core guarantee.** Every worker mutation is a full-state upsert keyed by the client-generated UUID. Replaying any request any number of times, in any interleaving, must converge to the same DB state. This has priority test coverage (property-style, real Postgres).
- **`tenant_id` from the authenticated principal on every domain query** (`auth.PrincipalFrom(ctx)`), never from URL/body. Worker's `user_id` is `principal.UserID`; role is `worker` (enforced by the middleware already).
- **Evidence is sacred:** photos and executions are insert-only in spirit (upsert, never destructive-drop of confirmed evidence); a missing/unconfirmed photo is surfaced explicitly, never silently discarded. The production S3 key has no DELETE — code must never rely on deleting S3 objects.
- **Suspension carve-out:** the execution upsert and both photo endpoints carry `auth.SuspendedWritable()` metadata so a suspended tenant's evidence still flushes (docs/12/08). The finer `device_finished_at < suspended_at` check is **deferred** — the `tenant` table has no `suspended_at` column and no ops `suspend` command sets one yet (phase 6); the app enters read-only mode on the first suspended response, so mutations received under suspension are legitimately pre-suspension outbox flushes. Documented, not silently dropped.
- **`finished_at` semantics (resolves the docs/07 vs docs/06 ambiguity toward docs/06):** the request carries device-clock `started_at` and `device_finished_at` (null while in progress). The server stores those verbatim and stamps `finished_at` = **server receipt time**, set **once** when `device_finished_at` first becomes non-null (via `COALESCE`, so replays never change it — required for idempotency). Reports use device time; disputes can reference both (docs/06 decision 2). The wiring task updates the `docs/07` payload example to use `device_finished_at`.
- **Presigned URL lifetimes:** 15 min PUT (spec §7). The API issues JSON only and never streams photo bytes; the client PUTs directly to S3.
- **RFC 7807 problem+json with stable `type` slugs:** reuse `auth`'s helper pattern. New slugs: `work-order-not-assigned` (403, order not assigned to this worker), `execution-not-found` (404), `photo-not-uploaded` (409, confirm before the S3 object exists), `object-not-found` (404), `qr-not-found` (404). Slugs are API contract.
- **Never hand-edit generated code** (`apps/api/internal/db/`, `packages/api-client/`). Each endpoint-adding task runs `make generate-client` and commits the regenerated `openapi.json` + `sdano.ts` so `make drift` stays green.
- **Zero golangci-lint warnings**; `slog` only; errors wrapped with context at boundaries; conventional commits authored as `Vladislav Bogatyrev <vladislav.bogatyrev@gmail.com>` ending with a `Co-Authored-By: Claude Fable 5 <noreply@anthropic.com>` trailer.
- **Tests hitting Postgres use testcontainers.** Before `go test`, every task exports `DOCKER_HOST=unix://$(podman machine inspect --format '{{.ConnectionInfo.PodmanSocket.Path}}')` and `TESTCONTAINERS_RYUK_DISABLED=true`. Go commands run from `apps/api/`; `make` from repo root.
- Module path `sdano.app/api`. No schema migration is needed — all tables (`work_order`, `work_execution`, `work_execution_item`, `photo`, `object`) already exist from migration 0001.

## Consumed interfaces (from merged auth, verify before use)

- `auth.PrincipalFrom(ctx context.Context) (auth.Principal, bool)`; `auth.Principal{UserID, TenantID uuid.UUID; Role auth.Role}`.
- `auth.Public() map[string]any`, `auth.SuspendedWritable() map[string]any` (operation metadata).
- `app.Deps{Pool *pgxpool.Pool; Checks []HealthCheck}` — extended with `S3 *s3.Client` in the photo task.
- `db.New(pool) *db.Queries`; `app.NewS3(cfg) (*s3.Client, error)`; `db.Queries.WithTx(tx) *db.Queries`.
- Handler-registration pattern: see `apps/api/internal/object/http.go` (`Register(api huma.API, queries *db.Queries)`), wired in `apps/api/internal/app/app.go`.

## Task index

1. sqlc worker queries (all)
2. `GET /worker/today` — bootstrap read
3. `PUT /worker/executions/{id}` — idempotent full-state upsert (core; property tests)
4. Photo two-phase upload — presign + confirm (adds S3 to Deps)
5. `GET /worker/objects/by-qr/{qr_token}` — QR resolution
6. Wiring polish, docs sync, final verification sweep

Detailed tasks follow.

---

### Task 1: sqlc worker queries

**Files:**
- Create: `db/queries/worker.sql`
- Generated (committed, never hand-edited): `apps/api/internal/db/worker.sql.go`

**Interfaces:**
- Consumes: the schema from migration 0001 (`work_order`, `work_execution`, `work_execution_item`, `photo`, `object`, `checklist_template_item`); enums `db.WorkOrderStatus`, `db.PhotoKind`.
- Produces (later tasks rely on these — trust the generated file if a field type differs; nullable timestamptz → `pgtype.Timestamptz`, nullable text/float → `*string`/`*float64`, uuid → `uuid.UUID`):
  - `ListWorkerTodayOrders(ctx, ListWorkerTodayOrdersParams) ([]ListWorkerTodayOrdersRow, error)` — params `TenantID, AssigneeID uuid.UUID; DueDate pgtype.Date`.
  - `ListChecklistItemsByVersions(ctx, versionIDs []uuid.UUID) ([]ListChecklistItemsByVersionsRow, error)`.
  - `GetWorkOrderForWorker(ctx, GetWorkOrderForWorkerParams{ID, TenantID}) (GetWorkOrderForWorkerRow, error)` — row has `AssigneeID uuid.NullUUID, VersionID, ObjectID uuid.UUID, Status db.WorkOrderStatus`.
  - `UpsertWorkExecution(ctx, UpsertWorkExecutionParams) error` — params `ID, TenantID, WorkOrderID, WorkerID uuid.UUID; StartedAt, DeviceFinishedAt pgtype.Timestamptz; Note *string`.
  - `DeleteExecutionItemsNotIn(ctx, DeleteExecutionItemsNotInParams{ExecutionID uuid.UUID; KeepIds []uuid.UUID}) error`.
  - `UpsertWorkExecutionItem(ctx, UpsertWorkExecutionItemParams) error` — params `ID, ExecutionID, TemplateItemID uuid.UUID; Checked bool; CheckedAt pgtype.Timestamptz`.
  - `SetWorkOrderStatus(ctx, SetWorkOrderStatusParams{ID, TenantID uuid.UUID; Status db.WorkOrderStatus}) error`.
  - `GetExecution(ctx, GetExecutionParams{ID, TenantID}) (GetExecutionRow, error)`.
  - `GetExecutionForWorker(ctx, GetExecutionForWorkerParams{ID, TenantID}) (GetExecutionForWorkerRow{ID, TenantID, WorkerID uuid.UUID}, error)`.
  - `ListExecutionItems(ctx, executionID uuid.UUID) ([]ListExecutionItemsRow, error)`.
  - `ListExecutionPhotos(ctx, executionID uuid.NullUUID) ([]ListExecutionPhotosRow, error)`.
  - `InsertPhotoPresign(ctx, InsertPhotoPresignParams{ID, TenantID uuid.UUID; ExecutionID uuid.NullUUID; Kind db.PhotoKind; S3Key string}) error`.
  - `GetPhoto(ctx, GetPhotoParams{ID, TenantID}) (GetPhotoRow, error)`.
  - `ConfirmPhoto(ctx, ConfirmPhotoParams{ID, TenantID uuid.UUID; TakenAt pgtype.Timestamptz; Lat, Lon *float64}) error`.
  - `GetObjectByQr(ctx, GetObjectByQrParams{TenantID uuid.UUID; QrToken *string}) (GetObjectByQrRow, error)`.
  - `GetWorkerOrderForObject(ctx, GetWorkerOrderForObjectParams) (GetWorkerOrderForObjectRow, error)`.

- [ ] **Step 1: Write `db/queries/worker.sql`**

```sql
-- === today bootstrap =======================================================

-- name: ListWorkerTodayOrders :many
SELECT wo.id, wo.object_id, wo.due_date, wo.status, wo.version_id,
       o.name AS object_name, o.address, o.lat, o.lon, o.qr_token
FROM work_order wo
JOIN object o ON o.id = wo.object_id
WHERE wo.tenant_id = $1 AND wo.assignee_id = $2 AND wo.due_date = $3
ORDER BY o.name;

-- name: ListChecklistItemsByVersions :many
SELECT version_id, id, position, title, requires_photo
FROM checklist_template_item
WHERE version_id = ANY(sqlc.arg(version_ids)::uuid[])
ORDER BY version_id, position;

-- === execution upsert ======================================================

-- name: GetWorkOrderForWorker :one
SELECT id, object_id, assignee_id, status, version_id
FROM work_order
WHERE id = $1 AND tenant_id = $2;

-- name: UpsertWorkExecution :exec
-- Full-state idempotent upsert. finished_at is server receipt time, stamped
-- once when device_finished_at first becomes non-null (COALESCE keeps it stable
-- across replays). The ON CONFLICT WHERE guard prevents a colliding id from a
-- different tenant/worker overwriting an existing row (defense in depth).
INSERT INTO work_execution (
    id, tenant_id, work_order_id, worker_id, started_at, device_finished_at, finished_at, note
) VALUES (
    sqlc.arg(id), sqlc.arg(tenant_id), sqlc.arg(work_order_id), sqlc.arg(worker_id),
    sqlc.arg(started_at), sqlc.arg(device_finished_at),
    CASE WHEN sqlc.arg(device_finished_at)::timestamptz IS NOT NULL THEN now() ELSE NULL END,
    sqlc.arg(note)
)
ON CONFLICT (id) DO UPDATE SET
    started_at         = EXCLUDED.started_at,
    device_finished_at = EXCLUDED.device_finished_at,
    finished_at        = CASE
        WHEN EXCLUDED.device_finished_at IS NULL THEN NULL
        ELSE COALESCE(work_execution.finished_at, now())
    END,
    note = EXCLUDED.note
WHERE work_execution.tenant_id = EXCLUDED.tenant_id
  AND work_execution.worker_id = EXCLUDED.worker_id;

-- name: DeleteExecutionItemsNotIn :exec
DELETE FROM work_execution_item
WHERE execution_id = $1 AND id <> ALL(sqlc.arg(keep_ids)::uuid[]);

-- name: UpsertWorkExecutionItem :exec
INSERT INTO work_execution_item (id, execution_id, template_item_id, checked, checked_at)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO UPDATE SET
    checked    = EXCLUDED.checked,
    checked_at = EXCLUDED.checked_at
WHERE work_execution_item.execution_id = EXCLUDED.execution_id;

-- name: SetWorkOrderStatus :exec
UPDATE work_order SET status = $3 WHERE id = $1 AND tenant_id = $2;

-- === execution read (server view) ==========================================

-- name: GetExecution :one
SELECT id, work_order_id, worker_id, started_at, finished_at, device_finished_at, note
FROM work_execution
WHERE id = $1 AND tenant_id = $2;

-- name: GetExecutionForWorker :one
SELECT id, tenant_id, worker_id
FROM work_execution
WHERE id = $1 AND tenant_id = $2;

-- name: ListExecutionItems :many
SELECT id, template_item_id, checked, checked_at
FROM work_execution_item
WHERE execution_id = $1
ORDER BY id;

-- name: ListExecutionPhotos :many
SELECT id, kind, taken_at, lat, lon, uploaded_at
FROM photo
WHERE execution_id = $1
ORDER BY id;

-- === photos (two-phase) ====================================================

-- name: InsertPhotoPresign :exec
INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (id) DO NOTHING;

-- name: GetPhoto :one
SELECT id, tenant_id, execution_id, kind, s3_key, taken_at, lat, lon, uploaded_at
FROM photo
WHERE id = $1 AND tenant_id = $2;

-- name: ConfirmPhoto :exec
-- uploaded_at stamped once (COALESCE); taken_at/lat/lon are device values,
-- deterministic across replays.
UPDATE photo
SET uploaded_at = COALESCE(uploaded_at, now()),
    taken_at    = $3,
    lat         = $4,
    lon         = $5
WHERE id = $1 AND tenant_id = $2;

-- === QR resolution =========================================================

-- name: GetObjectByQr :one
SELECT id, name, address, lat, lon, kind, qr_token, is_active
FROM object
WHERE tenant_id = $1 AND qr_token = $2;

-- name: GetWorkerOrderForObject :one
SELECT id, object_id, due_date, status, version_id
FROM work_order
WHERE tenant_id = $1 AND assignee_id = $2 AND object_id = $3 AND due_date = $4
ORDER BY created_at DESC
LIMIT 1;
```

- [ ] **Step 2: Generate and verify it compiles**

```bash
make generate-sqlc
cd apps/api && go build ./...
```
Expected: `apps/api/internal/db/worker.sql.go` created; build clean. Inspect the generated signatures — confirm they match the Interfaces list above (especially the `[]uuid.UUID` array params on `ListChecklistItemsByVersions`, `DeleteExecutionItemsNotIn`, and that `UpsertWorkExecution` has a single `DeviceFinishedAt` param despite `sqlc.arg(device_finished_at)` appearing twice). If any nullable column mapped to a different type than the Interfaces block predicts, note it in your report — later tasks adapt to the generated types, never edit the generated file.

- [ ] **Step 3: Lint**

```bash
golangci-lint run
```
Expected: zero warnings.

- [ ] **Step 4: Commit (generated code included)**

```bash
git add db/queries/worker.sql apps/api/internal/db/worker.sql.go
git commit -m "feat(worker): sqlc queries for today, executions, photos, qr"
```

---

### Task 2: `GET /worker/today` — bootstrap read

**Files:**
- Create: `apps/api/internal/workorder/http.go`
- Test: `apps/api/internal/workorder/http_test.go`
- Modify: `apps/api/internal/app/app.go` (register the route)
- Generated (committed): `packages/api-client/openapi.json`, `packages/api-client/src/generated/sdano.ts`

**Interfaces:**
- Consumes: `ListWorkerTodayOrders`, `ListChecklistItemsByVersions` (Task 1); `auth.PrincipalFrom`.
- Produces: `workorder.Register(api huma.API, pool *pgxpool.Pool)` (grows in Task 3, which needs the pool for transactions); operation `workerToday` at `GET /api/v1/worker/today`.

- [ ] **Step 1: Write the failing integration test**

`apps/api/internal/workorder/http_test.go`:
```go
package workorder_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"sdano.app/api/internal/app"
	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/testdb"
)

const testSecret = "worker-test-secret-at-least-32-bytes!!"

func workerBearer(t *testing.T, tenant, worker uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: worker, TenantID: tenant, Role: auth.RoleWorker}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("issue token: %v", err)
	}
	return "Bearer " + tok
}

func TestWorkerTodayReturnsAssignedRoute(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")

	tenant, worker := uuid.New(), uuid.New()
	object := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	item1, item2 := uuid.New(), uuid.New()
	order := uuid.New()
	otherWorker := uuid.New()

	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Alexey')`, worker, tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Other')`, otherWorker, tenant)
	must(`INSERT INTO object (id, tenant_id, name, address, qr_token) VALUES ($1,$2,'Lenina 45','Lenina 45','QR-LENINA')`, object, tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'Bus stop')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title, requires_photo) VALUES ($1,$2,1,'Collect trash',false)`, item1, version)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title, requires_photo) VALUES ($1,$2,2,'Wash shelter',true)`, item2, version)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, order, tenant, object, version, worker, today)
	// A second order for another worker on the same day must NOT appear.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, uuid.New(), tenant, object, version, otherWorker, today)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})

	req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/today", nil)
	req.Header.Set("Authorization", workerBearer(t, tenant, worker))
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("today: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	for _, want := range []string{"Lenina 45", "QR-LENINA", "Collect trash", "Wash shelter", `"version_id"`, order.String()} {
		if !strings.Contains(body, want) {
			t.Errorf("today body missing %q; body: %s", want, body)
		}
	}
	// The other worker's route count: exactly one work_order for this worker.
	if n := strings.Count(body, `"object_id"`); n != 1 {
		t.Errorf("expected exactly 1 work order for this worker, saw %d; body: %s", n, body)
	}
}
```
- [ ] **Step 2: Run the test to verify it fails**

Set the podman env, then:
Run: `go test ./internal/workorder/`
Expected: FAIL — package `workorder` does not exist / route 404.

- [ ] **Step 3: Implement `http.go`**

`apps/api/internal/workorder/http.go`:
```go
// Package workorder exposes the worker-facing planned-loop endpoints: the
// today bootstrap read and the idempotent execution upsert.
package workorder

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

// Register wires the worker planned-loop routes onto api. It takes the pool
// (not just Queries) because the execution upsert added in Task 3 runs a
// transaction. Route registration never touches the pool until a request runs,
// so a nil pool (openapi mode) is fine.
func Register(api huma.API, pool *pgxpool.Pool) {
	q := db.New(pool)
	registerToday(api, q)
}

type todayObject struct {
	ID      uuid.UUID `json:"id"`
	Name    string    `json:"name"`
	Address *string   `json:"address"`
	Lat     *float64  `json:"lat"`
	Lon     *float64  `json:"lon"`
	QRToken *string   `json:"qr_token"`
}

type checklistItem struct {
	ID            uuid.UUID `json:"id"`
	Position      int32     `json:"position"`
	Title         string    `json:"title"`
	RequiresPhoto bool      `json:"requires_photo"`
}

type checklist struct {
	VersionID uuid.UUID       `json:"version_id"`
	Items     []checklistItem `json:"items"`
}

type todayOrder struct {
	ID        uuid.UUID `json:"id"`
	ObjectID  uuid.UUID `json:"object_id"`
	DueDate   string    `json:"due_date"`
	Status    string    `json:"status"`
	Checklist checklist `json:"checklist"`
}

type todayOutput struct {
	Body struct {
		GeneratedAt time.Time     `json:"generated_at"`
		Objects     []todayObject `json:"objects"`
		WorkOrders  []todayOrder  `json:"work_orders"`
	}
}

func registerToday(api huma.API, q *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "workerToday",
		Method:      http.MethodGet,
		Path:        "/api/v1/worker/today",
		Summary:     "Worker's working set for today",
		Tags:        []string{"worker"},
	}, func(ctx context.Context, _ *struct{}) (*todayOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		now := time.Now().UTC()
		orders, err := q.ListWorkerTodayOrders(ctx, db.ListWorkerTodayOrdersParams{
			TenantID:   p.TenantID,
			AssigneeID: p.UserID,
			DueDate:    pgtype.Date{Time: now, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("listing today orders for worker %s: %w", p.UserID, err)
		}

		versionSet := map[uuid.UUID]struct{}{}
		for _, o := range orders {
			versionSet[o.VersionID] = struct{}{}
		}
		versionIDs := make([]uuid.UUID, 0, len(versionSet))
		for v := range versionSet {
			versionIDs = append(versionIDs, v)
		}
		itemsByVersion := map[uuid.UUID][]checklistItem{}
		if len(versionIDs) > 0 {
			items, err := q.ListChecklistItemsByVersions(ctx, versionIDs)
			if err != nil {
				return nil, fmt.Errorf("listing checklist items: %w", err)
			}
			for _, it := range items {
				itemsByVersion[it.VersionID] = append(itemsByVersion[it.VersionID], checklistItem{
					ID: it.ID, Position: it.Position, Title: it.Title, RequiresPhoto: it.RequiresPhoto,
				})
			}
		}

		out := &todayOutput{}
		out.Body.GeneratedAt = now
		out.Body.Objects = make([]todayObject, 0, len(orders))
		out.Body.WorkOrders = make([]todayOrder, 0, len(orders))
		seenObject := map[uuid.UUID]struct{}{}
		for _, o := range orders {
			if _, dup := seenObject[o.ObjectID]; !dup {
				seenObject[o.ObjectID] = struct{}{}
				out.Body.Objects = append(out.Body.Objects, todayObject{
					ID: o.ObjectID, Name: o.ObjectName, Address: o.Address,
					Lat: o.Lat, Lon: o.Lon, QRToken: o.QrToken,
				})
			}
			items := itemsByVersion[o.VersionID]
			if items == nil {
				items = []checklistItem{}
			}
			out.Body.WorkOrders = append(out.Body.WorkOrders, todayOrder{
				ID: o.ID, ObjectID: o.ObjectID,
				DueDate:   o.DueDate.Time.Format("2006-01-02"),
				Status:    string(o.Status),
				Checklist: checklist{VersionID: o.VersionID, Items: items},
			})
		}
		return out, nil
	})
}
```
(If the generated `ListWorkerTodayOrdersRow` field names differ — e.g. `ObjectName` vs `Object_name` — adapt to the generated names; never edit the generated file.)

- [ ] **Step 4: Register the route in `app.go`**

In `apps/api/internal/app/app.go`, after `object.Register(api, queries)`, add:
```go
	workorder.Register(api, deps.Pool)
```
with import `sdano.app/api/internal/workorder`.

- [ ] **Step 5: Run the test to verify it passes**

Run: `go test ./internal/workorder/ ./internal/app/`
Expected: PASS.

- [ ] **Step 6: Regenerate the client + drift**

From repo root:
```bash
make generate-client && npm run typecheck -w packages/api-client && git add packages/api-client && make drift
```
Expected: `openapi.json` gains `workerToday`; typecheck clean; `make drift` exits 0.

- [ ] **Step 7: Lint + commit**

```bash
cd apps/api && golangci-lint run && cd ..
git add apps/api/internal/workorder/ apps/api/internal/app/app.go packages/api-client
git commit -m "feat(worker): GET /worker/today bootstrap read"
```

---

### Task 3: `PUT /worker/executions/{id}` — idempotent full-state upsert

**Files:**
- Create: `apps/api/internal/workorder/execution.go` (service + view builder)
- Modify: `apps/api/internal/workorder/http.go` (add `registerExecutions`, `problem` helper, output types)
- Test: `apps/api/internal/workorder/execution_test.go` (property-style, direct service)
- Test: extend `apps/api/internal/workorder/http_test.go` (HTTP round-trip)
- Generated (committed): `packages/api-client/openapi.json`, `packages/api-client/src/generated/sdano.ts`

**Interfaces:**
- Consumes: `GetWorkOrderForWorker`, `UpsertWorkExecution`, `DeleteExecutionItemsNotIn`, `UpsertWorkExecutionItem`, `SetWorkOrderStatus`, `GetExecution`, `ListExecutionItems`, `ListExecutionPhotos` (Task 1); `db.WorkOrderStatusInProgress`/`db.WorkOrderStatusDone` enum constants; `auth.SuspendedWritable`.
- Produces:
  - `UpsertExecution(ctx context.Context, pool *pgxpool.Pool, tenantID, workerID, executionID uuid.UUID, in ExecutionInput) error` (exported for property tests).
  - `ExecutionInput{WorkOrderID uuid.UUID; StartedAt, DeviceFinishedAt *time.Time; Note *string; Items []ExecutionItemInput}`; `ExecutionItemInput{ID, TemplateItemID uuid.UUID; Checked bool; CheckedAt *time.Time}`.
  - `var ErrWorkOrderNotAssigned = errors.New(...)`.
  - operation `upsertWorkerExecution` at `PUT /api/v1/worker/executions/{id}` with `auth.SuspendedWritable()` metadata.

- [ ] **Step 1: Write the failing property test**

`apps/api/internal/workorder/execution_test.go`:
```go
package workorder_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/testdb"
	"sdano.app/api/internal/workorder"
)

type fixture struct {
	tenant, worker, order       uuid.UUID
	tmplItem1, tmplItem2        uuid.UUID // template_item ids
	execItem1, execItem2        uuid.UUID // client-generated work_execution_item ids
}

func seedExecutionFixture(t *testing.T, pool *pgxpool.Pool, assignee uuid.UUID) fixture {
	t.Helper()
	ctx := context.Background()
	f := fixture{
		tenant: uuid.New(), worker: assignee, order: uuid.New(),
		tmplItem1: uuid.New(), tmplItem2: uuid.New(),
		execItem1: uuid.New(), execItem2: uuid.New(),
	}
	object := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id, name) VALUES ($1,'Acme')`, f.tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','A')`, assignee, f.tenant)
	must(`INSERT INTO object (id, tenant_id, name) VALUES ($1,$2,'Obj')`, object, f.tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, f.tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title) VALUES ($1,$2,1,'i1')`, f.tmplItem1, version)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title) VALUES ($1,$2,2,'i2')`, f.tmplItem2, version)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,current_date)`, f.order, f.tenant, object, version, assignee)
	return f
}

// countItems returns the current work_execution_item ids for an execution.
func execItemIDs(t *testing.T, pool *pgxpool.Pool, execID uuid.UUID) map[uuid.UUID]bool {
	t.Helper()
	rows, err := pool.Query(context.Background(), `SELECT id FROM work_execution_item WHERE execution_id=$1`, execID)
	if err != nil {
		t.Fatalf("query items: %v", err)
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[id] = true
	}
	return out
}

func TestExecutionUpsertIsIdempotent(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedExecutionFixture(t, pool, uuid.New())
	execID := uuid.New()
	done := time.Now().UTC().Add(-time.Hour)
	snapshot := workorder.ExecutionInput{
		WorkOrderID:      f.order,
		StartedAt:        &done,
		DeviceFinishedAt: &done,
		Items: []workorder.ExecutionItemInput{
			{ID: f.execItem1, TemplateItemID: f.tmplItem1, Checked: true, CheckedAt: &done},
			{ID: f.execItem2, TemplateItemID: f.tmplItem2, Checked: true, CheckedAt: &done},
		},
	}
	// Apply once, capture server finished_at, then replay 3x and assert it never changes.
	if err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, snapshot); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	var firstFinished time.Time
	if err := pool.QueryRow(ctx, `SELECT finished_at FROM work_execution WHERE id=$1`, execID).Scan(&firstFinished); err != nil {
		t.Fatalf("read finished_at: %v", err)
	}
	for i := 0; i < 3; i++ {
		if err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, snapshot); err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
	}
	var afterFinished time.Time
	if err := pool.QueryRow(ctx, `SELECT finished_at FROM work_execution WHERE id=$1`, execID).Scan(&afterFinished); err != nil {
		t.Fatalf("read finished_at after: %v", err)
	}
	if !firstFinished.Equal(afterFinished) {
		t.Errorf("finished_at changed on replay: %v -> %v (not idempotent)", firstFinished, afterFinished)
	}
	items := execItemIDs(t, pool, execID)
	if len(items) != 2 || !items[f.execItem1] || !items[f.execItem2] {
		t.Errorf("item set drifted on replay: %v", items)
	}
	var status string
	_ = pool.QueryRow(ctx, `SELECT status FROM work_order WHERE id=$1`, f.order).Scan(&status)
	if status != "done" {
		t.Errorf("order status = %q, want done", status)
	}
}

func TestExecutionUpsertLastWriteWins(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedExecutionFixture(t, pool, uuid.New())
	execID := uuid.New()
	start := time.Now().UTC().Add(-2 * time.Hour)

	// A: in progress, both items present and checked.
	a := workorder.ExecutionInput{
		WorkOrderID: f.order, StartedAt: &start,
		Items: []workorder.ExecutionItemInput{
			{ID: f.execItem1, TemplateItemID: f.tmplItem1, Checked: true},
			{ID: f.execItem2, TemplateItemID: f.tmplItem2, Checked: true},
		},
	}
	// B: item2 removed from the snapshot, item1 unchecked.
	b := workorder.ExecutionInput{
		WorkOrderID: f.order, StartedAt: &start,
		Items: []workorder.ExecutionItemInput{
			{ID: f.execItem1, TemplateItemID: f.tmplItem1, Checked: false},
		},
	}
	for _, snap := range []workorder.ExecutionInput{a, b, a, b} { // ends on B
		if err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, snap); err != nil {
			t.Fatalf("apply: %v", err)
		}
	}
	items := execItemIDs(t, pool, execID)
	if len(items) != 1 || !items[f.execItem1] || items[f.execItem2] {
		t.Errorf("last-write-wins failed: want only execItem1, got %v", items)
	}
	var checked bool
	_ = pool.QueryRow(ctx, `SELECT checked FROM work_execution_item WHERE id=$1`, f.execItem1).Scan(&checked)
	if checked {
		t.Error("item1 should be unchecked after final snapshot B")
	}
}

func TestExecutionUpsertRejectsUnassignedOrder(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedExecutionFixture(t, pool, uuid.New()) // order assigned to f.worker
	intruder := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','X')`, intruder, f.tenant); err != nil {
		t.Fatalf("seed intruder: %v", err)
	}
	err := workorder.UpsertExecution(ctx, pool, f.tenant, intruder, uuid.New(), workorder.ExecutionInput{WorkOrderID: f.order})
	if !errors.Is(err, workorder.ErrWorkOrderNotAssigned) {
		t.Errorf("intruder must be rejected: got %v", err)
	}
}
```

- [ ] **Step 2: Run the test to verify it fails**

Set the podman env, then:
Run: `go test ./internal/workorder/ -run TestExecutionUpsert`
Expected: FAIL — `undefined: workorder.UpsertExecution`.

- [ ] **Step 3: Implement `execution.go`**

`apps/api/internal/workorder/execution.go`:
```go
package workorder

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
)

// ErrWorkOrderNotAssigned is returned when a worker tries to execute an order
// that is not theirs (or does not exist for their tenant).
var ErrWorkOrderNotAssigned = errors.New("work order not assigned to this worker")

type ExecutionItemInput struct {
	ID             uuid.UUID
	TemplateItemID uuid.UUID
	Checked        bool
	CheckedAt      *time.Time
}

type ExecutionInput struct {
	WorkOrderID      uuid.UUID
	StartedAt        *time.Time
	DeviceFinishedAt *time.Time
	Note             *string
	Items            []ExecutionItemInput
}

func ts(t *time.Time) pgtype.Timestamptz {
	if t == nil {
		return pgtype.Timestamptz{}
	}
	return pgtype.Timestamptz{Time: *t, Valid: true}
}

func tptr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// UpsertExecution validates the order belongs to the worker, then applies the
// full-state snapshot (execution + items + order status) in one transaction.
// Idempotent by construction: replaying any snapshot converges. finished_at is
// stamped once (server clock) when device_finished_at first appears.
func UpsertExecution(ctx context.Context, pool *pgxpool.Pool, tenantID, workerID, executionID uuid.UUID, in ExecutionInput) error {
	q := db.New(pool)
	wo, err := q.GetWorkOrderForWorker(ctx, db.GetWorkOrderForWorkerParams{ID: in.WorkOrderID, TenantID: tenantID})
	if errors.Is(err, pgx.ErrNoRows) {
		return ErrWorkOrderNotAssigned
	}
	if err != nil {
		return fmt.Errorf("loading work order: %w", err)
	}
	if !wo.AssigneeID.Valid || wo.AssigneeID.UUID != workerID {
		return ErrWorkOrderNotAssigned
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	qtx := q.WithTx(tx)

	if err := qtx.UpsertWorkExecution(ctx, db.UpsertWorkExecutionParams{
		ID: executionID, TenantID: tenantID, WorkOrderID: in.WorkOrderID, WorkerID: workerID,
		StartedAt: ts(in.StartedAt), DeviceFinishedAt: ts(in.DeviceFinishedAt), Note: in.Note,
	}); err != nil {
		return fmt.Errorf("upsert execution: %w", err)
	}

	keep := make([]uuid.UUID, 0, len(in.Items))
	for _, it := range in.Items {
		keep = append(keep, it.ID)
	}
	if err := qtx.DeleteExecutionItemsNotIn(ctx, db.DeleteExecutionItemsNotInParams{ExecutionID: executionID, KeepIds: keep}); err != nil {
		return fmt.Errorf("pruning items: %w", err)
	}
	for _, it := range in.Items {
		if err := qtx.UpsertWorkExecutionItem(ctx, db.UpsertWorkExecutionItemParams{
			ID: it.ID, ExecutionID: executionID, TemplateItemID: it.TemplateItemID,
			Checked: it.Checked, CheckedAt: ts(it.CheckedAt),
		}); err != nil {
			return fmt.Errorf("upsert item %s: %w", it.ID, err)
		}
	}

	status := db.WorkOrderStatusInProgress
	if in.DeviceFinishedAt != nil {
		status = db.WorkOrderStatusDone
	}
	if err := qtx.SetWorkOrderStatus(ctx, db.SetWorkOrderStatusParams{ID: in.WorkOrderID, TenantID: tenantID, Status: status}); err != nil {
		return fmt.Errorf("set order status: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ExecutionView is the server's view of an execution, returned by the upsert.
type ExecutionView struct {
	ID               uuid.UUID            `json:"id"`
	WorkOrderID      uuid.UUID            `json:"work_order_id"`
	StartedAt        *time.Time           `json:"started_at"`
	FinishedAt       *time.Time           `json:"finished_at"`
	DeviceFinishedAt *time.Time           `json:"device_finished_at"`
	Note             *string              `json:"note"`
	Items            []executionItemView  `json:"items"`
	Photos           []executionPhotoView `json:"photos"`
}

type executionItemView struct {
	ID             uuid.UUID  `json:"id"`
	TemplateItemID uuid.UUID  `json:"template_item_id"`
	Checked        bool       `json:"checked"`
	CheckedAt      *time.Time `json:"checked_at"`
}

type executionPhotoView struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	TakenAt    *time.Time `json:"taken_at"`
	Lat        *float64   `json:"lat"`
	Lon        *float64   `json:"lon"`
	UploadedAt *time.Time `json:"uploaded_at"`
}

func loadExecutionView(ctx context.Context, q *db.Queries, tenantID, executionID uuid.UUID) (ExecutionView, error) {
	e, err := q.GetExecution(ctx, db.GetExecutionParams{ID: executionID, TenantID: tenantID})
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading execution: %w", err)
	}
	items, err := q.ListExecutionItems(ctx, executionID)
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading items: %w", err)
	}
	photos, err := q.ListExecutionPhotos(ctx, uuid.NullUUID{UUID: executionID, Valid: true})
	if err != nil {
		return ExecutionView{}, fmt.Errorf("loading photos: %w", err)
	}
	v := ExecutionView{
		ID: e.ID, WorkOrderID: e.WorkOrderID,
		StartedAt: tptr(e.StartedAt), FinishedAt: tptr(e.FinishedAt), DeviceFinishedAt: tptr(e.DeviceFinishedAt),
		Note:   e.Note,
		Items:  make([]executionItemView, 0, len(items)),
		Photos: make([]executionPhotoView, 0, len(photos)),
	}
	for _, it := range items {
		v.Items = append(v.Items, executionItemView{ID: it.ID, TemplateItemID: it.TemplateItemID, Checked: it.Checked, CheckedAt: tptr(it.CheckedAt)})
	}
	for _, ph := range photos {
		v.Photos = append(v.Photos, executionPhotoView{ID: ph.ID, Kind: string(ph.Kind), TakenAt: tptr(ph.TakenAt), Lat: ph.Lat, Lon: ph.Lon, UploadedAt: tptr(ph.UploadedAt)})
	}
	return v, nil
}
```
(Verify the generated enum constant names `db.WorkOrderStatusInProgress`/`db.WorkOrderStatusDone` — if sqlc named them differently, use `db.WorkOrderStatus("in_progress")`/`db.WorkOrderStatus("done")`.)

- [ ] **Step 4: Run the property tests to verify they pass**

Run: `go test ./internal/workorder/ -run TestExecutionUpsert`
Expected: PASS (idempotent, last-write-wins, unassigned-rejected).

- [ ] **Step 5: Add the HTTP handler in `http.go`**

Append to `apps/api/internal/workorder/http.go`: extend `Register` to also call `registerExecutions(api, pool)`, and add the handler + a `problem` helper. Change the `Register` body to:
```go
func Register(api huma.API, pool *pgxpool.Pool) {
	q := db.New(pool)
	registerToday(api, q)
	registerExecutions(api, pool)
}
```
Add these to `http.go` (imports: add `"errors"`, `"github.com/jackc/pgx/v5/pgxpool"`):
```go
func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

type executionItemBody struct {
	ID             string     `json:"id" format:"uuid"`
	TemplateItemID string     `json:"template_item_id" format:"uuid"`
	Checked        bool       `json:"checked"`
	CheckedAt      *time.Time `json:"checked_at"`
}

type executionUpsertInput struct {
	ID   string `path:"id"`
	Body struct {
		WorkOrderID      string              `json:"work_order_id" format:"uuid"`
		StartedAt        *time.Time          `json:"started_at"`
		DeviceFinishedAt *time.Time          `json:"device_finished_at"`
		Note             *string             `json:"note"`
		Items            []executionItemBody `json:"items"`
	}
}

type executionOutput struct {
	Body ExecutionView
}

func registerExecutions(api huma.API, pool *pgxpool.Pool) {
	huma.Register(api, huma.Operation{
		OperationID: "upsertWorkerExecution",
		Method:      http.MethodPut,
		Path:        "/api/v1/worker/executions/{id}",
		Summary:     "Idempotent full-state execution upsert",
		Tags:        []string{"worker"},
		Metadata:    auth.SuspendedWritable(),
	}, func(ctx context.Context, in *executionUpsertInput) (*executionOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		execID, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid execution id")
		}
		woID, err := uuid.Parse(in.Body.WorkOrderID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid work_order_id")
		}
		items := make([]ExecutionItemInput, 0, len(in.Body.Items))
		for _, it := range in.Body.Items {
			iid, err := uuid.Parse(it.ID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid item id")
			}
			tid, err := uuid.Parse(it.TemplateItemID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid template_item_id")
			}
			items = append(items, ExecutionItemInput{ID: iid, TemplateItemID: tid, Checked: it.Checked, CheckedAt: it.CheckedAt})
		}
		if err := UpsertExecution(ctx, pool, p.TenantID, p.UserID, execID, ExecutionInput{
			WorkOrderID: woID, StartedAt: in.Body.StartedAt, DeviceFinishedAt: in.Body.DeviceFinishedAt,
			Note: in.Body.Note, Items: items,
		}); errors.Is(err, ErrWorkOrderNotAssigned) {
			return nil, problem(http.StatusForbidden, "work-order-not-assigned", "this work order is not assigned to you")
		} else if err != nil {
			return nil, err
		}
		view, err := loadExecutionView(ctx, db.New(pool), p.TenantID, execID)
		if err != nil {
			return nil, err
		}
		return &executionOutput{Body: view}, nil
	})
}
```

- [ ] **Step 6: Add the HTTP round-trip test**

Append to `apps/api/internal/workorder/http_test.go` (reuse `seedExecutionFixture`, `workerBearer`):
```go
func TestExecutionUpsertHTTPRoundTrip(t *testing.T) {
	pool := testdb.New(t)
	worker := uuid.New()
	f := seedExecutionFixture(t, pool, worker)
	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	execID := uuid.New()

	body := `{"work_order_id":"` + f.order.String() + `","started_at":"2026-07-09T09:00:00Z","items":[{"id":"` + f.execItem1.String() + `","template_item_id":"` + f.tmplItem1.String() + `","checked":true}]}`
	put := func(authz string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodPut, "/api/v1/worker/executions/"+execID.String(), strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if authz != "" {
			req.Header.Set("Authorization", authz)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := put(workerBearer(t, f.tenant, worker))
	if rec.Code != http.StatusOK {
		t.Fatalf("upsert: got %d; body %s", rec.Code, rec.Body)
	}
	if !strings.Contains(rec.Body.String(), f.execItem1.String()) {
		t.Errorf("server view must echo the item; body %s", rec.Body)
	}
	// Replay is safe (idempotent) → still 200.
	if rec2 := put(workerBearer(t, f.tenant, worker)); rec2.Code != http.StatusOK {
		t.Errorf("replay: got %d", rec2.Code)
	}
	// A different worker in the same tenant is forbidden (order not theirs).
	intruder := uuid.New()
	if _, err := pool.Exec(context.Background(), `INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','I')`, intruder, f.tenant); err != nil {
		t.Fatalf("seed intruder: %v", err)
	}
	if rec3 := put(workerBearer(t, f.tenant, intruder)); rec3.Code != http.StatusForbidden {
		t.Errorf("intruder: got %d, want 403; body %s", rec3.Code, rec3.Body)
	}
}
```

- [ ] **Step 7: Run all workorder tests + lint**

```bash
go test ./internal/workorder/ ./internal/app/ && golangci-lint run
```
Expected: PASS, zero warnings.

- [ ] **Step 8: Regenerate client + drift**

```bash
cd .. && make generate-client && npm run typecheck -w packages/api-client && git add packages/api-client && make drift
```
Expected: `openapi.json` gains `upsertWorkerExecution`; drift exits 0.

- [ ] **Step 9: Commit**

```bash
git add apps/api/internal/workorder/ packages/api-client
git commit -m "feat(worker): idempotent PUT /worker/executions/{id} full-state upsert"
```

---

### Task 4: Photo two-phase upload — presign + confirm

**Files:**
- Create: `apps/api/internal/photo/store.go` (ObjectStore interface + S3 impl)
- Create: `apps/api/internal/photo/http.go` (presign + confirm handlers)
- Test: `apps/api/internal/photo/http_test.go` (fake store; real DB)
- Modify: `apps/api/internal/app/app.go` (`Deps.S3`, register photo routes), `apps/api/cmd/api/main.go` (wire S3 into Deps)
- Generated (committed): `packages/api-client/openapi.json`, `packages/api-client/src/generated/sdano.ts`

**Interfaces:**
- Consumes: `GetExecutionForWorker`, `InsertPhotoPresign`, `GetPhoto`, `ConfirmPhoto` (Task 1); `auth.PrincipalFrom`, `auth.SuspendedWritable`; `app.NewS3`.
- Produces:
  - `photo.ObjectStore` interface: `PresignPut(ctx, key, contentType string) (url string, expiresAt time.Time, err error)`; `Exists(ctx, key string) (bool, error)`.
  - `photo.NewS3Store(client *s3.Client, bucket string) *S3Store` (implements ObjectStore).
  - `photo.Register(api huma.API, pool *pgxpool.Pool, store ObjectStore)`.
  - operations `presignWorkerPhoto` (`POST /api/v1/worker/photos/presign`) and `confirmWorkerPhoto` (`POST /api/v1/worker/photos/{id}/confirm`), both `auth.SuspendedWritable()`.
  - `app.Deps` gains `S3 *s3.Client`.

- [ ] **Step 1: Implement the ObjectStore (`store.go`)**

`apps/api/internal/photo/store.go`:
```go
// Package photo exposes the worker two-phase photo upload: presign (issue a
// direct-to-S3 PUT URL) and confirm (verify the object landed, then stamp the
// row). The API never streams photo bytes.
package photo

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// presignTTL is the lifetime of a presigned PUT (spec §7: 15 min PUT).
const presignTTL = 15 * time.Minute

// ObjectStore is the slice of object-storage behaviour the photo handlers need.
// Injecting it keeps the handlers testable without a live S3.
type ObjectStore interface {
	PresignPut(ctx context.Context, key, contentType string) (url string, expiresAt time.Time, err error)
	Exists(ctx context.Context, key string) (bool, error)
}

// S3Store implements ObjectStore against an aws-sdk-go-v2 S3 client.
type S3Store struct {
	client *s3.Client
	bucket string
}

func NewS3Store(client *s3.Client, bucket string) *S3Store {
	return &S3Store{client: client, bucket: bucket}
}

func (s *S3Store) PresignPut(ctx context.Context, key, contentType string) (string, time.Time, error) {
	req, err := s3.NewPresignClient(s.client).PresignPutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		ContentType: &contentType,
	}, s3.WithPresignExpires(presignTTL))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presigning put %s: %w", key, err)
	}
	return req.URL, time.Now().Add(presignTTL), nil
}

func (s *S3Store) Exists(ctx context.Context, key string) (bool, error) {
	_, err := s.client.HeadObject(ctx, &s3.HeadObjectInput{Bucket: &s.bucket, Key: &key})
	if err == nil {
		return true, nil
	}
	var notFound *types.NotFound
	if errors.As(err, &notFound) {
		return false, nil
	}
	// MinIO / some backends return a generic 404 APIError code instead of NotFound.
	var apiErr smithy.APIError
	if errors.As(err, &apiErr) && (apiErr.ErrorCode() == "NotFound" || apiErr.ErrorCode() == "NoSuchKey") {
		return false, nil
	}
	return false, fmt.Errorf("head object %s: %w", key, err)
}
```

- [ ] **Step 2: Write the failing handler test (fake store)**

`apps/api/internal/photo/http_test.go`:
```go
package photo_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/config"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/photo"
	"sdano.app/api/internal/testdb"

	"github.com/danielgtaylor/huma/v2/humatest"
)

const testSecret = "photo-test-secret-at-least-32-bytes!!"

// fakeStore records presign calls and answers Exists from a set.
type fakeStore struct {
	exists map[string]bool
}

func (f *fakeStore) PresignPut(_ context.Context, key, _ string) (string, time.Time, error) {
	return "https://s3.example/" + key + "?sig=x", time.Now().Add(15 * time.Minute), nil
}
func (f *fakeStore) Exists(_ context.Context, key string) (bool, error) {
	return f.exists[key], nil
}

// seedExecution inserts a tenant, worker, object, version, order, and an
// execution owned by the worker. Returns tenant, worker, executionID.
func seedExecution(t *testing.T, pool *pgxpool.Pool) (tenant, worker, exec uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, worker, exec = uuid.New(), uuid.New(), uuid.New()
	object, tmpl, version, order := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name) VALUES ($1,$2,'O')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,current_date)`, order, tenant, object, version, worker)
	must(`INSERT INTO work_execution (id,tenant_id,work_order_id,worker_id) VALUES ($1,$2,$3,$4)`, exec, tenant, order, worker)
	return tenant, worker, exec
}

func buildPhotoAPI(t *testing.T, pool *pgxpool.Pool, store photo.ObjectStore) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t)
	a := auth.NewAuthenticator(testSecret, db.New(pool))
	api.UseMiddleware(a.Authenticate, a.Authorize)
	photo.Register(api, pool, store)
	return api
}

func workerBearer(t *testing.T, tenant, worker uuid.UUID) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(testSecret, auth.Principal{UserID: worker, TenantID: tenant, Role: auth.RoleWorker}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return "Bearer " + tok
}

func TestPhotoPresignThenConfirm(t *testing.T) {
	pool := testdb.New(t)
	tenant, worker, exec := seedExecution(t, pool)
	store := &fakeStore{exists: map[string]bool{}}
	api := buildPhotoAPI(t, pool, store)
	bearer := workerBearer(t, tenant, worker)
	photoID := uuid.New()

	// Presign creates the photo row and returns a URL for the deterministic key.
	presignBody := map[string]any{"id": photoID.String(), "execution_id": exec.String(), "kind": "after", "content_type": "image/jpeg"}
	pres := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody)
	if pres.Code != http.StatusOK {
		t.Fatalf("presign: got %d; body %s", pres.Code, pres.Body)
	}
	wantKey := "tenants/" + tenant.String() + "/photos/" + photoID.String() + ".jpg"
	if !strings.Contains(pres.Body.String(), wantKey) {
		t.Errorf("presign body must contain the s3 key %q; body %s", wantKey, pres.Body)
	}

	// Confirm before the object exists → 409 photo-not-uploaded.
	confirmBody := map[string]any{"taken_at": "2026-07-09T09:00:00Z", "lat": 55.75, "lon": 37.61}
	if c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, confirmBody); c.Code != http.StatusConflict || !strings.Contains(c.Body.String(), "photo-not-uploaded") {
		t.Fatalf("confirm before upload: got %d body %s", c.Code, c.Body)
	}

	// Object lands in S3 → confirm succeeds and stamps uploaded_at.
	store.exists[wantKey] = true
	c := api.Post("/api/v1/worker/photos/"+photoID.String()+"/confirm", "Authorization: "+bearer, confirmBody)
	if c.Code != http.StatusOK {
		t.Fatalf("confirm: got %d; body %s", c.Code, c.Body)
	}
	var uploaded *time.Time
	if err := pool.QueryRow(context.Background(), `SELECT uploaded_at FROM photo WHERE id=$1`, photoID).Scan(&uploaded); err != nil || uploaded == nil {
		t.Fatalf("uploaded_at not stamped: %v %v", uploaded, err)
	}
	// Re-presign an unconfirmed-or-confirmed photo is idempotent (same key, 200).
	if p2 := api.Post("/api/v1/worker/photos/presign", "Authorization: "+bearer, presignBody); p2.Code != http.StatusOK {
		t.Errorf("re-presign: got %d", p2.Code)
	}
}
```
- [ ] **Step 3: Run the test to verify it fails**

Set the podman env, then:
Run: `go test ./internal/photo/`
Expected: FAIL — `undefined: photo.Register`.

- [ ] **Step 4: Implement `http.go`**

`apps/api/internal/photo/http.go`:
```go
package photo

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/danielgtaylor/huma/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
)

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

func s3Key(tenantID, photoID uuid.UUID) string {
	return fmt.Sprintf("tenants/%s/photos/%s.jpg", tenantID, photoID)
}

func ptr(v pgtype.Timestamptz) *time.Time {
	if !v.Valid {
		return nil
	}
	t := v.Time
	return &t
}

// Register wires the two-phase photo routes.
func Register(api huma.API, pool *pgxpool.Pool, store ObjectStore) {
	q := db.New(pool)
	registerPresign(api, q, store)
	registerConfirm(api, q, store)
}

type presignInput struct {
	Body struct {
		ID          string `json:"id" format:"uuid"`
		ExecutionID string `json:"execution_id" format:"uuid"`
		Kind        string `json:"kind" enum:"before,after,defect,resolution"`
		ContentType string `json:"content_type" example:"image/jpeg"`
	}
}

type presignOutput struct {
	Body struct {
		UploadURL string    `json:"upload_url"`
		S3Key     string    `json:"s3_key"`
		ExpiresAt time.Time `json:"expires_at"`
	}
}

func registerPresign(api huma.API, q *db.Queries, store ObjectStore) {
	huma.Register(api, huma.Operation{
		OperationID: "presignWorkerPhoto",
		Method:      http.MethodPost,
		Path:        "/api/v1/worker/photos/presign",
		Summary:     "Presign a direct-to-S3 photo upload",
		Tags:        []string{"worker"},
		Metadata:    auth.SuspendedWritable(),
	}, func(ctx context.Context, in *presignInput) (*presignOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		photoID, err := uuid.Parse(in.Body.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid photo id")
		}
		execID, err := uuid.Parse(in.Body.ExecutionID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid execution_id")
		}
		ex, err := q.GetExecutionForWorker(ctx, db.GetExecutionForWorkerParams{ID: execID, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "execution-not-found", "execution not found")
		}
		if err != nil {
			return nil, fmt.Errorf("loading execution: %w", err)
		}
		if ex.WorkerID != p.UserID {
			return nil, problem(http.StatusForbidden, "work-order-not-assigned", "execution belongs to another worker")
		}
		key := s3Key(p.TenantID, photoID)
		if err := q.InsertPhotoPresign(ctx, db.InsertPhotoPresignParams{
			ID: photoID, TenantID: p.TenantID,
			ExecutionID: uuid.NullUUID{UUID: execID, Valid: true},
			Kind:        db.PhotoKind(in.Body.Kind), S3Key: key,
		}); err != nil {
			return nil, fmt.Errorf("inserting photo row: %w", err)
		}
		url, expires, err := store.PresignPut(ctx, key, in.Body.ContentType)
		if err != nil {
			return nil, fmt.Errorf("presigning: %w", err)
		}
		out := &presignOutput{}
		out.Body.UploadURL = url
		out.Body.S3Key = key
		out.Body.ExpiresAt = expires
		return out, nil
	})
}

type confirmInput struct {
	ID   string `path:"id"`
	Body struct {
		TakenAt *time.Time `json:"taken_at"`
		Lat     *float64   `json:"lat"`
		Lon     *float64   `json:"lon"`
	}
}

type photoView struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	TakenAt    *time.Time `json:"taken_at"`
	Lat        *float64   `json:"lat"`
	Lon        *float64   `json:"lon"`
	UploadedAt *time.Time `json:"uploaded_at"`
}

type confirmOutput struct {
	Body photoView
}

func registerConfirm(api huma.API, q *db.Queries, store ObjectStore) {
	huma.Register(api, huma.Operation{
		OperationID: "confirmWorkerPhoto",
		Method:      http.MethodPost,
		Path:        "/api/v1/worker/photos/{id}/confirm",
		Summary:     "Confirm a photo uploaded to S3",
		Tags:        []string{"worker"},
		Metadata:    auth.SuspendedWritable(),
	}, func(ctx context.Context, in *confirmInput) (*confirmOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		photoID, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid photo id")
		}
		ph, err := q.GetPhoto(ctx, db.GetPhotoParams{ID: photoID, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "photo-not-found", "photo not found")
		}
		if err != nil {
			return nil, fmt.Errorf("loading photo: %w", err)
		}
		exists, err := store.Exists(ctx, ph.S3Key)
		if err != nil {
			return nil, fmt.Errorf("checking object: %w", err)
		}
		if !exists {
			return nil, problem(http.StatusConflict, "photo-not-uploaded", "the object has not been uploaded yet")
		}
		if err := q.ConfirmPhoto(ctx, db.ConfirmPhotoParams{
			ID: photoID, TenantID: p.TenantID,
			TakenAt: func() pgtype.Timestamptz {
				if in.Body.TakenAt == nil {
					return pgtype.Timestamptz{}
				}
				return pgtype.Timestamptz{Time: *in.Body.TakenAt, Valid: true}
			}(),
			Lat: in.Body.Lat, Lon: in.Body.Lon,
		}); err != nil {
			return nil, fmt.Errorf("confirming photo: %w", err)
		}
		fresh, err := q.GetPhoto(ctx, db.GetPhotoParams{ID: photoID, TenantID: p.TenantID})
		if err != nil {
			return nil, fmt.Errorf("reloading photo: %w", err)
		}
		out := &confirmOutput{Body: photoView{
			ID: fresh.ID, Kind: string(fresh.Kind), TakenAt: ptr(fresh.TakenAt),
			Lat: fresh.Lat, Lon: fresh.Lon, UploadedAt: ptr(fresh.UploadedAt),
		}}
		return out, nil
	})
}
```

- [ ] **Step 5: Add `S3` to `Deps` and wire it**

In `apps/api/internal/app/app.go`, extend `Deps`:
```go
type Deps struct {
	Pool   *pgxpool.Pool
	S3     *s3.Client
	Checks []HealthCheck
}
```
(add import `"github.com/aws/aws-sdk-go-v2/service/s3"`). In `app.New`, after `workorder.Register(api, deps.Pool)`, add:
```go
	photo.Register(api, deps.Pool, photo.NewS3Store(deps.S3, cfg.S3Bucket))
```
(import `sdano.app/api/internal/photo`). In `apps/api/cmd/api/main.go`, the `run` func already builds `s3c` — add it to `Deps`:
```go
	router, _ := app.New(cfg, app.Deps{
		Pool: pool,
		S3:   s3c,
		Checks: []app.HealthCheck{
			app.DBCheck(pool),
			app.S3Check(s3c, cfg.S3Bucket),
		},
	})
```

- [ ] **Step 6: Run tests + lint**

```bash
go test ./internal/photo/ ./internal/app/ && golangci-lint run
```
Expected: PASS, zero warnings.

- [ ] **Step 7: Regenerate client + drift + commit**

```bash
cd .. && make generate-client && npm run typecheck -w packages/api-client && git add packages/api-client && make drift
git add apps/api/internal/photo/ apps/api/internal/app/app.go apps/api/cmd/api/main.go packages/api-client
git commit -m "feat(worker): two-phase photo presign + confirm"
```

---

### Task 5: `GET /worker/objects/by-qr/{qr_token}` — QR resolution

**Files:**
- Modify: `apps/api/internal/object/http.go` (add the worker QR route to `Register`)
- Test: `apps/api/internal/object/http_test.go` (append a QR test)
- Generated (committed): `packages/api-client/openapi.json`, `packages/api-client/src/generated/sdano.ts`

**Interfaces:**
- Consumes: `GetObjectByQr`, `GetWorkerOrderForObject` (Task 1); `auth.PrincipalFrom`.
- Produces: operation `workerObjectByQR` at `GET /api/v1/worker/objects/by-qr/{qr_token}`. `object.Register` (already called in app.go) also registers this route — no app.go change.

- [ ] **Step 1: Append the failing test to `apps/api/internal/object/http_test.go`**

```go
func TestWorkerObjectByQR(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	today := time.Now().UTC().Format("2006-01-02")
	tenant, worker := uuid.New(), uuid.New()
	object := uuid.New()
	tmpl, version, order := uuid.New(), uuid.New(), uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO tenant (id,name) VALUES ($1,'Acme')`, tenant)
	must(`INSERT INTO app_user (id,tenant_id,role,display_name) VALUES ($1,$2,'worker','A')`, worker, tenant)
	must(`INSERT INTO object (id,tenant_id,name,qr_token) VALUES ($1,$2,'Lenina 45','QR-XYZ')`, object, tenant)
	must(`INSERT INTO checklist_template (id,tenant_id,name) VALUES ($1,$2,'T')`, tmpl, tenant)
	must(`INSERT INTO checklist_template_version (id,template_id,version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO work_order (id,tenant_id,object_id,version_id,assignee_id,due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, order, tenant, object, version, worker, today)

	router, _ := app.New(config.Config{JWTSecret: testSecret}, app.Deps{Pool: pool})
	get := func(token string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/api/v1/worker/objects/by-qr/"+token, nil)
		req.Header.Set("Authorization", bearerAs(t, tenant, worker, auth.RoleWorker))
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	rec := get("QR-XYZ")
	if rec.Code != http.StatusOK {
		t.Fatalf("qr resolve: got %d; body %s", rec.Code, rec.Body)
	}
	body := rec.Body.String()
	if !strings.Contains(body, "Lenina 45") || !strings.Contains(body, order.String()) {
		t.Errorf("qr body must carry the object and today's order; body %s", body)
	}
	// Unknown QR → 404.
	if rec404 := get("QR-NOPE"); rec404.Code != http.StatusNotFound {
		t.Errorf("unknown qr: got %d, want 404", rec404.Code)
	}
}
```
Note: the existing `bearer(t, tenant, role)` helper in `object/http_test.go` mints a token for the given role and tenant; it currently signs a random `UserID`. For this test the QR order is assigned to `worker`, but `GetWorkerOrderForObject` keys on `assignee_id = principal.UserID`. Update `bearer` to accept an explicit user id, OR add a `bearerAs(t, tenant, worker, role)` variant that signs `UserID: worker`, and use it here so the order resolves. (Minimal: add `bearerAs` and call it with `worker`.)

- [ ] **Step 2: Run the test to verify it fails**

Set the podman env, then:
Run: `go test ./internal/object/ -run TestWorkerObjectByQR`
Expected: FAIL — route 404 (QR endpoint not registered).

- [ ] **Step 3: Add the QR route to `object/http.go`**

In `apps/api/internal/object/http.go`, extend `Register` to also register the QR route, and add the handler + types. Add to `Register`'s body (after the existing `listStaffObjects` registration):
```go
	registerWorkerQR(api, queries)
```
Add (imports needed: `"errors"`, `"time"`, `"github.com/jackc/pgx/v5"`):
```go
func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

type qrObject struct {
	ID       uuid.UUID `json:"id"`
	Name     string    `json:"name"`
	Address  *string   `json:"address"`
	Lat      *float64  `json:"lat"`
	Lon      *float64  `json:"lon"`
	Kind     *string   `json:"kind"`
	QRToken  *string   `json:"qr_token"`
	IsActive bool      `json:"is_active"`
}

type qrTodayOrder struct {
	ID        uuid.UUID `json:"id"`
	ObjectID  uuid.UUID `json:"object_id"`
	DueDate   string    `json:"due_date"`
	Status    string    `json:"status"`
	VersionID uuid.UUID `json:"version_id"`
}

type qrInput struct {
	QRToken string `path:"qr_token"`
}

type qrOutput struct {
	Body struct {
		Object         qrObject      `json:"object"`
		TodayWorkOrder *qrTodayOrder `json:"today_work_order"`
	}
}

func registerWorkerQR(api huma.API, queries *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "workerObjectByQR",
		Method:      http.MethodGet,
		Path:        "/api/v1/worker/objects/by-qr/{qr_token}",
		Summary:     "Resolve an object by its QR token",
		Tags:        []string{"worker"},
	}, func(ctx context.Context, in *qrInput) (*qrOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		token := in.QRToken
		obj, err := queries.GetObjectByQr(ctx, db.GetObjectByQrParams{TenantID: p.TenantID, QrToken: &token})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "qr-not-found", "no object for this QR token")
		}
		if err != nil {
			return nil, fmt.Errorf("resolving qr: %w", err)
		}
		out := &qrOutput{}
		out.Body.Object = qrObject{
			ID: obj.ID, Name: obj.Name, Address: obj.Address, Lat: obj.Lat, Lon: obj.Lon,
			Kind: obj.Kind, QRToken: obj.QrToken, IsActive: obj.IsActive,
		}
		wo, err := queries.GetWorkerOrderForObject(ctx, db.GetWorkerOrderForObjectParams{
			TenantID: p.TenantID, AssigneeID: p.UserID, ObjectID: obj.ID,
			DueDate: pgtype.Date{Time: time.Now().UTC(), Valid: true},
		})
		if err == nil {
			out.Body.TodayWorkOrder = &qrTodayOrder{
				ID: wo.ID, ObjectID: wo.ObjectID, DueDate: wo.DueDate.Time.Format("2006-01-02"),
				Status: string(wo.Status), VersionID: wo.VersionID,
			}
		} else if !errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("resolving today order: %w", err)
		}
		return out, nil
	})
}
```
(Add `"github.com/jackc/pgx/v5/pgtype"` to the imports if not present.)

- [ ] **Step 4: Run tests + lint**

```bash
go test ./internal/object/ ./internal/app/ && golangci-lint run
```
Expected: PASS, zero warnings.

- [ ] **Step 5: Regenerate client + drift + commit**

```bash
cd .. && make generate-client && npm run typecheck -w packages/api-client && git add packages/api-client && make drift
git add apps/api/internal/object/ packages/api-client
git commit -m "feat(worker): GET /worker/objects/by-qr/{qr_token} resolution"
```

---

### Task 6: Wiring polish, docs sync, final verification sweep

**Files:**
- Modify: `docs/07-api-spec.md` (the executions payload example)
- Verification only otherwise.

- [ ] **Step 1: Sync the `docs/07` executions payload to the implemented shape**

In `docs/07-api-spec.md`, the `PUT /worker/executions/{id}` example currently shows `"finished_at"` as a device-clock field. Replace that payload block's `"finished_at"` line so the example matches the implemented contract (device-clock `device_finished_at`; server sets `finished_at`). Change:
```jsonc
  "started_at": "...",            // device clock
  "finished_at": "...",           // device clock; null while in progress
```
to:
```jsonc
  "started_at": "...",            // device clock
  "device_finished_at": "...",    // device clock at completion; null while in progress
```
And append after the "Full-state upsert" paragraph: "The server stamps `finished_at` (server receipt time) once when `device_finished_at` first appears; both are kept (docs/06 decision 2)." Commit:
```bash
git add docs/07-api-spec.md
git commit -m "docs(worker): align executions payload example with device_finished_at"
```

- [ ] **Step 2: Clean-room gate**

Set the podman env. From repo root:
```bash
make dev-down && make dev-up && make migrate-up
make lint && make test && make drift
```
Expected: every command exits 0.

- [ ] **Step 3: End-to-end worker flow against the running stack**

Seed a worker + invite, claim a device token, then drive the full worker loop. From repo root (the api container must be running; ensure local `.env` has `JWT_SECRET` ≥32 bytes):
```bash
psql() { docker compose -f deploy/docker-compose.yml --env-file .env exec -T postgres psql -U sdano -d sdano "$@"; }
TENANT=$(psql -tAc "INSERT INTO tenant (name) VALUES ('Worker Demo') RETURNING id" | tr -d '[:space:]')
WORKER=$(psql -tAc "INSERT INTO app_user (tenant_id, role, display_name) VALUES ('$TENANT','worker','Alexey') RETURNING id" | tr -d '[:space:]')
OBJ=$(psql -tAc "INSERT INTO object (tenant_id, name, qr_token) VALUES ('$TENANT','Lenina 45','QR-DEMO') RETURNING id" | tr -d '[:space:]')
TMPL=$(psql -tAc "INSERT INTO checklist_template (tenant_id, name) VALUES ('$TENANT','T') RETURNING id" | tr -d '[:space:]')
VER=$(psql -tAc "INSERT INTO checklist_template_version (template_id, version) VALUES ('$TMPL',1) RETURNING id" | tr -d '[:space:]')
psql -c "INSERT INTO checklist_template_item (version_id, position, title) VALUES ('$VER',1,'Collect trash')"
ORDER=$(psql -tAc "INSERT INTO work_order (tenant_id, object_id, version_id, assignee_id, due_date) VALUES ('$TENANT','$OBJ','$VER','$WORKER',current_date) RETURNING id" | tr -d '[:space:]')
psql -c "INSERT INTO worker_invite (tenant_id, user_id, code, expires_at) VALUES ('$TENANT','$WORKER','777888', now()+interval '1 hour')"

DEVTOK=$(curl -s -X POST http://localhost:8080/api/v1/auth/worker/claim -H 'content-type: application/json' -d '{"invite_code":"777888"}' | grep -o '"device_token":"[^"]*"' | cut -d'"' -f4)
echo "today:";   curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/api/v1/worker/today -H "Authorization: Bearer $DEVTOK"
echo "qr:";      curl -s -o /dev/null -w '%{http_code}\n' http://localhost:8080/api/v1/worker/objects/by-qr/QR-DEMO -H "Authorization: Bearer $DEVTOK"
EXEC=$(cat /proc/sys/kernel/random/uuid 2>/dev/null || uuidgen | tr 'A-Z' 'a-z')
echo "execution:"; curl -s -o /dev/null -w '%{http_code}\n' -X PUT http://localhost:8080/api/v1/worker/executions/$EXEC -H "Authorization: Bearer $DEVTOK" -H 'content-type: application/json' -d "{\"work_order_id\":\"$ORDER\",\"started_at\":\"2026-07-09T09:00:00Z\",\"device_finished_at\":\"2026-07-09T09:10:00Z\",\"items\":[]}"
```
Expected: `today` → 200, `qr` → 200, `execution` → 200. (Photo presign requires a reachable S3 endpoint from the host; the fake-store handler tests already prove that path, so a live photo PUT is optional here.)

- [ ] **Step 4: Verify tree + history**

```bash
git status --short   # empty
git log --oneline    # conventional commits, one concern each
```

- [ ] **Step 5: Report completion**

The worker API is done: `GET /worker/today`, idempotent `PUT /worker/executions/{id}`, two-phase photo upload, and QR resolution — all behind the worker role gate, with the three evidence-flush mutations honoring the suspension carve-out. Next by the spec: the **staff API** (phase 5) and **reports + ops** (phase 6). The deferred `device_finished_at < suspended_at` refinement lands with the ops `suspend` command (which adds `tenant.suspended_at`).

## Plan Self-Review (done at write time)

- **Spec phase-4 coverage:** `GET /worker/today` ✓ (T2), idempotent execution upsert ✓ (T3, property tests), photo presign/confirm ✓ (T4), QR resolution ✓ (T5). docs/07 payload aligned ✓ (T6). docs/08 offline contract: the server side (idempotent full-state upserts, resumable presign, evidence never dropped) is what this plan delivers; the device-side outbox is the mobile app (out of scope).
- **Known judgment calls:** (1) `finished_at` = server time stamped once (resolves docs/07-vs-docs/06 toward the data model, required for idempotency); (2) blanket `SuspendedWritable()` carve-out for evidence-flush mutations, with the `device_finished_at < suspended_at` refinement deferred to the ops `suspend` command (documented); (3) photo `Exists`/`PresignPut` behind an injectable `ObjectStore` so handlers test without live S3 (real `S3Store` exercised in the clean-room sweep).
- **Type consistency:** `workorder.Register(api, pool)`, `UpsertExecution(ctx, pool, tenantID, workerID, executionID, ExecutionInput)`, `ExecutionInput`/`ExecutionItemInput`, `photo.Register(api, pool, store)`, `photo.ObjectStore`, `app.Deps{Pool, S3, Checks}`, the `problem(status, slug, detail)` helper (defined per-package), and the sqlc query/param/row names are used consistently across tasks.
