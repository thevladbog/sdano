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

	// Cross-tenant isolation: prove another tenant's order does NOT appear.
	otherTenant := uuid.New()
	otherWorkerForOtherTenant := uuid.New()
	otherObject := uuid.New()
	otherTmpl, otherVersion, otherOrder := uuid.New(), uuid.New(), uuid.New()
	must(`INSERT INTO tenant (id, name) VALUES ($1, 'OtherCo')`, otherTenant)
	must(`INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'worker','Ivan')`, otherWorkerForOtherTenant, otherTenant)
	must(`INSERT INTO object (id, tenant_id, name) VALUES ($1,$2,'OTHER-TENANT-OBJECT')`, otherObject, otherTenant)
	must(`INSERT INTO checklist_template (id, tenant_id, name) VALUES ($1,$2,'OtherT')`, otherTmpl, otherTenant)
	must(`INSERT INTO checklist_template_version (id, template_id, version) VALUES ($1,$2,1)`, otherVersion, otherTmpl)
	must(`INSERT INTO work_order (id, tenant_id, object_id, version_id, assignee_id, due_date) VALUES ($1,$2,$3,$4,$5,$6::date)`, otherOrder, otherTenant, otherObject, otherVersion, otherWorkerForOtherTenant, today)

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
	// Cross-tenant isolation: other tenant's object must not appear.
	if strings.Contains(body, "OTHER-TENANT-OBJECT") {
		t.Errorf("tenant isolation broken — another tenant's object leaked into /worker/today; body: %s", body)
	}
	// The other worker's route count: exactly one work_order for this worker.
	if n := strings.Count(body, `"object_id"`); n != 1 {
		t.Errorf("expected exactly 1 work order for this worker, saw %d; body: %s", n, body)
	}
}
