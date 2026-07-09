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
	tenant, worker, order uuid.UUID
	tmplItem1, tmplItem2  uuid.UUID // template_item ids
	execItem1, execItem2  uuid.UUID // client-generated work_execution_item ids
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

// TestExecutionUpsertRejectsCrossTenantIDCollision proves that a colliding
// client-generated execution id (work_execution.id is a global PK with no
// per-tenant namespace, by design — see db/migrations "Client-generated
// UUIDs (offline idempotency)") cannot be used to prune or overwrite another
// tenant's execution items. UpsertWorkExecution's ON CONFLICT ... WHERE guard
// silently skips the row write on a mismatched tenant/worker; the service
// must detect that and abort before touching work_execution_item, which has
// no tenant scoping of its own.
func TestExecutionUpsertRejectsCrossTenantIDCollision(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()

	victim := seedExecutionFixture(t, pool, uuid.New())
	sharedExecID := uuid.New() // both tenants happen to pick the same client-generated id
	if err := workorder.UpsertExecution(ctx, pool, victim.tenant, victim.worker, sharedExecID, workorder.ExecutionInput{
		WorkOrderID: victim.order,
		Items: []workorder.ExecutionItemInput{
			{ID: victim.execItem1, TemplateItemID: victim.tmplItem1, Checked: true},
			{ID: victim.execItem2, TemplateItemID: victim.tmplItem2, Checked: true},
		},
	}); err != nil {
		t.Fatalf("seed victim execution: %v", err)
	}

	attacker := seedExecutionFixture(t, pool, uuid.New())
	err := workorder.UpsertExecution(ctx, pool, attacker.tenant, attacker.worker, sharedExecID, workorder.ExecutionInput{
		WorkOrderID: attacker.order, // attacker's own, legitimately assigned order
		Items:       nil,            // empty snapshot: if this weren't blocked, it would prune ALL of the victim's items
	})
	if !errors.Is(err, workorder.ErrExecutionIDConflict) {
		t.Fatalf("cross-tenant id collision must be rejected: got %v", err)
	}

	items := execItemIDs(t, pool, sharedExecID)
	if len(items) != 2 || !items[victim.execItem1] || !items[victim.execItem2] {
		t.Errorf("victim's execution items must survive the attacker's colliding-id request: %v", items)
	}
}

// TestExecutionUpsertRejectsWorkOrderRepointing proves that reusing an
// existing execution id under a *different* one of the worker's own assigned
// orders is rejected. UpsertWorkExecution's ON CONFLICT DO UPDATE never
// changes work_order_id, so without this check the request would half-apply:
// order2's status would flip (in-progress/done) while the execution row (and
// the response echoing it) stayed bound to order1.
func TestExecutionUpsertRejectsWorkOrderRepointing(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedExecutionFixture(t, pool, uuid.New())

	// A second work order, also assigned to f.worker in the same tenant.
	object2, version2, tmpl2 := uuid.New(), uuid.New(), uuid.New()
	order2 := uuid.New()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}
	must(`INSERT INTO object (id, tenant_id, name) VALUES ($1,$2,'Obj2')`, object2, f.tenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T2')`, tmpl2, f.tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version2, tmpl2)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,current_date)`,
		order2, f.tenant, object2, version2, f.worker)

	execID := uuid.New()
	if err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, workorder.ExecutionInput{
		WorkOrderID: f.order,
	}); err != nil {
		t.Fatalf("apply to order1: %v", err)
	}

	err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, workorder.ExecutionInput{
		WorkOrderID: order2,
	})
	if !errors.Is(err, workorder.ErrExecutionIDConflict) {
		t.Fatalf("re-pointing an execution to a different order must be rejected: got %v", err)
	}

	var status string
	if err := pool.QueryRow(ctx, `SELECT status FROM work_order WHERE id=$1`, order2).Scan(&status); err != nil {
		t.Fatalf("read order2 status: %v", err)
	}
	if status != "scheduled" {
		t.Errorf("order2 status = %q, want scheduled (must not be flipped by the rejected request)", status)
	}
}

// TestExecutionUpsertRejectsForeignTemplateItem proves that an execution item
// whose template_item_id does not belong to the work order's pinned checklist
// version is rejected. work_execution_item.template_item_id is a global FK
// with no per-tenant/per-version scoping, so without this check a worker
// could bind another tenant's (or another version's) template item into
// their own execution.
func TestExecutionUpsertRejectsForeignTemplateItem(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	f := seedExecutionFixture(t, pool, uuid.New())
	execID := uuid.New()

	foreignTemplateItem := uuid.New() // not part of f.order's checklist version
	err := workorder.UpsertExecution(ctx, pool, f.tenant, f.worker, execID, workorder.ExecutionInput{
		WorkOrderID: f.order,
		Items: []workorder.ExecutionItemInput{
			{ID: uuid.New(), TemplateItemID: foreignTemplateItem, Checked: true},
		},
	})
	if !errors.Is(err, workorder.ErrInvalidChecklistItem) {
		t.Fatalf("foreign template item must be rejected: got %v", err)
	}
}
