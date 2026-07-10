package report_test

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/db"
	"sdano.app/api/internal/report"
	"sdano.app/api/internal/testdb"
)

// fakeLoader returns a fixed 1x1 gif data URI and counts how many times it
// was invoked, so tests can assert the loader is skipped for unconfirmed
// photos (docs/09: missing photos render as an explicit placeholder, never a
// silent skip — and never touch S3 for evidence that was never uploaded).
func fakeLoader(calls *int) report.PhotoLoader {
	return func(_ context.Context, _ string) (string, error) {
		*calls++
		return "data:image/gif;base64,R0lGODlhAQABAIAAAAAAAP///yH5BAEAAAAALAAAAAABAAEAAAIBTAA7", nil
	}
}

// seedReportFixture builds the scenario described in the task-2 brief: two
// objects under one contract, three orders inside the report period (two
// done, one missed) plus a fourth order outside the period and a fifth under
// a different contract — both of which must be excluded from the result.
type reportFixture struct {
	tenant, contract1, contract2 uuid.UUID
	object1, object2, object3    uuid.UUID
	exec1, exec2                 uuid.UUID
	periodFrom, periodTo         time.Time
}

func seedReportFixture(t *testing.T, pool *pgxpool.Pool) reportFixture {
	t.Helper()
	ctx := context.Background()
	must := func(q string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, q, args...); err != nil {
			t.Fatalf("exec %s: %v", q, err)
		}
	}

	f := reportFixture{
		tenant:     uuid.New(),
		contract1:  uuid.New(),
		contract2:  uuid.New(),
		object1:    uuid.New(),
		object2:    uuid.New(),
		object3:    uuid.New(),
		exec1:      uuid.New(),
		exec2:      uuid.New(),
		periodFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		periodTo:   time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
	}

	worker := uuid.New()
	tmpl, version := uuid.New(), uuid.New()
	item1, item2 := uuid.New(), uuid.New()
	order1, order2, order3, order4, order5 := uuid.New(), uuid.New(), uuid.New(), uuid.New(), uuid.New()

	must(`INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, f.tenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Алексей Петров')`, worker, f.tenant)
	must(`INSERT INTO contract (id, tenant_id, name, client_name) VALUES ($1,$2,'Договор №1','Администрация')`, f.contract1, f.tenant)
	must(`INSERT INTO contract (id, tenant_id, name, client_name) VALUES ($1,$2,'Договор №2','Другой клиент')`, f.contract2, f.tenant)

	// object1 sorts AFTER object2 by address ('ул. Мира' > 'ул. Ленина') even
	// though it is inserted first — proves BuildReportData relies on the
	// query's ORDER BY, not insertion order.
	must(`INSERT INTO object (id, tenant_id, name, address, contract_id) VALUES ($1,$2,'Восточный сквер','ул. Мира, 5',$3)`, f.object1, f.tenant, f.contract1)
	must(`INSERT INTO object (id, tenant_id, name, address, contract_id) VALUES ($1,$2,'Западная остановка','ул. Ленина, 10',$3)`, f.object2, f.tenant, f.contract1)
	must(`INSERT INTO object (id, tenant_id, name, address, contract_id) VALUES ($1,$2,'Чужой объект','ул. Чужая, 1',$3)`, f.object3, f.tenant, f.contract2)

	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'T')`, tmpl, f.tenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, version, tmpl)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title) VALUES ($1,$2,1,'Item 1')`, item1, version)
	must(`INSERT INTO checklist_template_item (id, version_id, position, title) VALUES ($1,$2,2,'Item 2')`, item2, version)

	// order1: object1, done, 2/2 items checked, in period.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status) VALUES ($1,$2,$3,$4,$5,'2026-06-05','done')`,
		order1, f.tenant, f.object1, version, worker)
	must(`INSERT INTO work_execution (id, tenant_id, work_order_id, worker_id, device_finished_at, finished_at) VALUES ($1,$2,$3,$4,'2026-06-05T08:42:00Z','2026-06-05T08:43:00Z')`,
		f.exec1, f.tenant, order1, worker)
	must(`INSERT INTO work_execution_item (id, execution_id, template_item_id, checked) VALUES ($1,$2,$3,true)`, uuid.New(), f.exec1, item1)
	must(`INSERT INTO work_execution_item (id, execution_id, template_item_id, checked) VALUES ($1,$2,$3,true)`, uuid.New(), f.exec1, item2)

	// order2: object2, done, 1/2 items checked, in period.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status) VALUES ($1,$2,$3,$4,$5,'2026-06-10','done')`,
		order2, f.tenant, f.object2, version, worker)
	must(`INSERT INTO work_execution (id, tenant_id, work_order_id, worker_id, device_finished_at, finished_at) VALUES ($1,$2,$3,$4,'2026-06-10T09:05:00Z','2026-06-10T09:06:00Z')`,
		f.exec2, f.tenant, order2, worker)
	must(`INSERT INTO work_execution_item (id, execution_id, template_item_id, checked) VALUES ($1,$2,$3,true)`, uuid.New(), f.exec2, item1)
	must(`INSERT INTO work_execution_item (id, execution_id, template_item_id, checked) VALUES ($1,$2,$3,false)`, uuid.New(), f.exec2, item2)

	// order3: object1, missed, in period.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date, status) VALUES ($1,$2,$3,$4,$5,'2026-06-15','missed')`,
		order3, f.tenant, f.object1, version, worker)

	// order4: object1, scheduled, due OUTSIDE the period -> must be excluded.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,'2026-07-05')`,
		order4, f.tenant, f.object1, version, worker)

	// order5: object3, different contract, due INSIDE the period -> excluded by contract filter.
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,'2026-06-20')`,
		order5, f.tenant, f.object3, version, worker)

	// Photos on exec1: one confirmed (uploaded_at set), one unconfirmed (uploaded_at null).
	must(`INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key, taken_at, lat, lon, uploaded_at) VALUES ($1,$2,$3,'after','photos/exec1/after.jpg','2026-06-05T08:41:00Z',55.758,37.6173,'2026-06-05T08:41:30Z')`,
		uuid.New(), f.tenant, f.exec1)
	must(`INSERT INTO photo (id, tenant_id, execution_id, kind, s3_key, taken_at) VALUES ($1,$2,$3,'before','photos/exec1/before.jpg','2026-06-05T08:40:00Z')`,
		uuid.New(), f.tenant, f.exec1)

	return f
}

func TestBuildReportData(t *testing.T) {
	pool := testdb.New(t)
	f := seedReportFixture(t, pool)
	q := db.New(pool)

	var loaderCalls int
	data, err := report.BuildReportData(context.Background(), q, report.ClaimedReport{
		ID:         uuid.New(),
		TenantID:   f.tenant,
		ContractID: uuid.NullUUID{UUID: f.contract1, Valid: true},
		PeriodFrom: f.periodFrom,
		PeriodTo:   f.periodTo,
	}, fakeLoader(&loaderCalls))
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}

	if data.Summary.Planned != 3 {
		t.Errorf("Planned = %d, want 3", data.Summary.Planned)
	}
	if data.Summary.Done != 2 {
		t.Errorf("Done = %d, want 2", data.Summary.Done)
	}
	if data.Summary.Missed != 1 {
		t.Errorf("Missed = %d, want 1", data.Summary.Missed)
	}
	if data.Summary.CompletionPct != 66 {
		t.Errorf("CompletionPct = %d, want 66", data.Summary.CompletionPct)
	}

	// Per-object rows sorted by address, NOT insertion order.
	if len(data.Summary.PerObject) != 2 {
		t.Fatalf("PerObject len = %d, want 2", len(data.Summary.PerObject))
	}
	if data.Summary.PerObject[0].Address != "ул. Ленина, 10" {
		t.Errorf("PerObject[0].Address = %q, want ул. Ленина, 10 (sorted first)", data.Summary.PerObject[0].Address)
	}
	if data.Summary.PerObject[1].Address != "ул. Мира, 5" {
		t.Errorf("PerObject[1].Address = %q, want ул. Мира, 5 (sorted second)", data.Summary.PerObject[1].Address)
	}

	// Outside-period and other-contract orders must never surface anywhere.
	for _, obj := range data.Objects {
		if obj.Name == "Чужой объект" {
			t.Errorf("other-contract object leaked into report: %+v", obj)
		}
	}
	if len(data.Objects) != 2 {
		t.Fatalf("Objects len = %d, want 2", len(data.Objects))
	}

	// data.Objects preserves the same address-sorted order as the summary.
	objByName := map[string]report.ObjectSection{}
	for _, o := range data.Objects {
		objByName[o.Name] = o
	}
	obj1, ok := objByName["Восточный сквер"]
	if !ok {
		t.Fatalf("object1 (Восточный сквер) missing from Objects")
	}
	obj2, ok := objByName["Западная остановка"]
	if !ok {
		t.Fatalf("object2 (Западная остановка) missing from Objects")
	}

	if len(obj1.Jobs) != 1 {
		t.Fatalf("object1 Jobs len = %d, want 1 (order4 outside period must be excluded)", len(obj1.Jobs))
	}
	job1 := obj1.Jobs[0]
	if job1.CheckedItems != 2 || job1.TotalItems != 2 {
		t.Errorf("object1 job CheckedItems/TotalItems = %d/%d, want 2/2", job1.CheckedItems, job1.TotalItems)
	}
	if job1.FinishedAt != "08:42" {
		t.Errorf("object1 job FinishedAt = %q, want 08:42 (device time, HH:MM)", job1.FinishedAt)
	}
	if job1.Date != "05.06.2026" {
		t.Errorf("object1 job Date = %q, want 05.06.2026", job1.Date)
	}
	if job1.WorkerName != "Алексей Петров" {
		t.Errorf("object1 job WorkerName = %q, want Алексей Петров", job1.WorkerName)
	}

	if len(job1.Photos) != 2 {
		t.Fatalf("object1 job Photos len = %d, want 2", len(job1.Photos))
	}
	var sawConfirmed, sawMissing bool
	for _, p := range job1.Photos {
		if p.Missing {
			sawMissing = true
			if p.DataURI != "" {
				t.Errorf("missing photo cell must not carry a DataURI, got %q", p.DataURI)
			}
		} else {
			sawConfirmed = true
			if p.DataURI == "" {
				t.Errorf("confirmed photo cell must carry a DataURI")
			}
		}
	}
	if !sawConfirmed || !sawMissing {
		t.Errorf("expected one confirmed and one missing photo cell, got %+v", job1.Photos)
	}
	if loaderCalls != 1 {
		t.Errorf("loaderCalls = %d, want 1 (loader must not be called for unconfirmed photos)", loaderCalls)
	}

	if len(obj2.Jobs) != 1 {
		t.Fatalf("object2 Jobs len = %d, want 1", len(obj2.Jobs))
	}
	job2 := obj2.Jobs[0]
	if job2.CheckedItems != 1 || job2.TotalItems != 2 {
		t.Errorf("object2 job CheckedItems/TotalItems = %d/%d, want 1/2", job2.CheckedItems, job2.TotalItems)
	}
	if len(job2.Photos) != 0 {
		t.Errorf("object2 job Photos len = %d, want 0", len(job2.Photos))
	}

	// Missed row.
	if len(data.Missed) != 1 {
		t.Fatalf("Missed len = %d, want 1", len(data.Missed))
	}
	if data.Missed[0].ObjectName != "Восточный сквер" || data.Missed[0].Date != "15.06.2026" {
		t.Errorf("Missed[0] = %+v, want {Восточный сквер 15.06.2026}", data.Missed[0])
	}

	if data.TenantName != "Acme" {
		t.Errorf("TenantName = %q, want Acme", data.TenantName)
	}
	if data.ContractName != "Договор №1" || data.ClientName != "Администрация" {
		t.Errorf("ContractName/ClientName = %q/%q, want Договор №1/Администрация", data.ContractName, data.ClientName)
	}
}

// TestBuildReportDataZeroPlanned proves CompletionPct is 0 (not a division
// panic) when a tenant has no work orders at all in the period.
func TestBuildReportDataZeroPlanned(t *testing.T) {
	pool := testdb.New(t)
	ctx := context.Background()
	tenant := uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Empty Co')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	q := db.New(pool)

	data, err := report.BuildReportData(ctx, q, report.ClaimedReport{
		ID:         uuid.New(),
		TenantID:   tenant,
		PeriodFrom: time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC),
		PeriodTo:   time.Date(2026, 6, 30, 0, 0, 0, 0, time.UTC),
	}, fakeLoader(new(int)))
	if err != nil {
		t.Fatalf("BuildReportData: %v", err)
	}
	if data.Summary.Planned != 0 || data.Summary.CompletionPct != 0 {
		t.Errorf("Planned/CompletionPct = %d/%d, want 0/0", data.Summary.Planned, data.Summary.CompletionPct)
	}
	if data.ContractName != "" || data.ClientName != "" {
		t.Errorf("ContractName/ClientName should stay empty with no contract, got %q/%q", data.ContractName, data.ClientName)
	}
}
