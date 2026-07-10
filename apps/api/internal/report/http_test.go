package report_test

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/danielgtaylor/huma/v2/humatest"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/auth"
	"sdano.app/api/internal/db"
	"sdano.app/api/internal/report"
	"sdano.app/api/internal/testdb"
)

const reportTestSecret = "report-http-test-secret-at-least-32b!!"

// fakeReportStore is a minimal photo.ObjectStore double: only PresignGet is
// ever exercised by these handlers (the render worker, tested separately in
// worker_test.go, is the only caller of Put).
type fakeReportStore struct{}

func (fakeReportStore) PresignPut(_ context.Context, _, _ string) (string, time.Time, error) {
	return "", time.Time{}, nil
}
func (fakeReportStore) Exists(_ context.Context, _ string) (bool, error) { return false, nil }
func (fakeReportStore) PresignGet(_ context.Context, key string) (string, time.Time, error) {
	return "https://s3.example/GET/" + key + "?sig=g", time.Now().Add(5 * time.Minute), nil
}
func (fakeReportStore) Get(_ context.Context, _ string) ([]byte, error)    { return nil, nil }
func (fakeReportStore) Put(_ context.Context, _, _ string, _ []byte) error { return nil }

func buildReportAPI(t *testing.T, pool *pgxpool.Pool) humatest.TestAPI {
	t.Helper()
	_, api := humatest.New(t)
	a := auth.NewAuthenticator(reportTestSecret, db.New(pool))
	api.UseMiddleware(a.Authenticate, a.Authorize)
	report.Register(api, pool, fakeReportStore{})
	return api
}

// seedTenantAndAdmin inserts a tenant and one admin app_user (InsertReport's
// generated_by column is FK-constrained to app_user, so the token's UserID
// must resolve to a real row).
func seedTenantAndAdmin(t *testing.T, pool *pgxpool.Pool) (tenant, admin uuid.UUID) {
	t.Helper()
	ctx := context.Background()
	tenant, admin = uuid.New(), uuid.New()
	if _, err := pool.Exec(ctx, `INSERT INTO tenant (id, name) VALUES ($1, 'Acme')`, tenant); err != nil {
		t.Fatalf("seed tenant: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app_user (id, tenant_id, role, display_name) VALUES ($1,$2,'admin','Admin')`, admin, tenant); err != nil {
		t.Fatalf("seed admin: %v", err)
	}
	return tenant, admin
}

func bearerFor(t *testing.T, tenant, user uuid.UUID, role auth.Role) string {
	t.Helper()
	tok, err := auth.IssueAccessToken(reportTestSecret, auth.Principal{UserID: user, TenantID: tenant, Role: role}, auth.AccessTTL)
	if err != nil {
		t.Fatalf("token: %v", err)
	}
	return "Bearer " + tok
}

func decodeJSON(t *testing.T, rec *httptest.ResponseRecorder, v any) {
	t.Helper()
	if err := json.Unmarshal(rec.Body.Bytes(), v); err != nil {
		t.Fatalf("decode %s: %v", rec.Body, err)
	}
}

// TestReportCreatePollFlipReadyAndList covers the brief's step-1 happy path
// end to end: 202 create, poll while generating (no download_url), flip the
// row to ready via direct SQL (standing in for the render worker), poll
// again (download_url + url_expires_at present via the fake store's
// PresignGet), and confirm the report shows up in the history list.
func TestReportCreatePollFlipReadyAndList(t *testing.T) {
	pool := testdb.New(t)
	tenant, admin := seedTenantAndAdmin(t, pool)
	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, admin, auth.RoleAdmin)

	create := api.Post("/api/v1/staff/reports", bearer, map[string]any{
		"period_from": "2026-06-01",
		"period_to":   "2026-06-30",
	})
	if create.Code != http.StatusAccepted {
		t.Fatalf("create: got %d; body %s", create.Code, create.Body)
	}
	var created struct {
		ReportID uuid.UUID `json:"report_id"`
		Status   string    `json:"status"`
	}
	decodeJSON(t, create, &created)
	if created.Status != "generating" {
		t.Fatalf("create status = %q, want generating", created.Status)
	}

	// Poll while still generating: no download_url, no failure_reason.
	poll := api.Get("/api/v1/staff/reports/"+created.ReportID.String(), bearer)
	if poll.Code != http.StatusOK {
		t.Fatalf("get: got %d; body %s", poll.Code, poll.Body)
	}
	if strings.Contains(poll.Body.String(), "download_url") || strings.Contains(poll.Body.String(), "failure_reason") {
		t.Fatalf("generating report must not carry download_url/failure_reason: %s", poll.Body)
	}
	if !strings.Contains(poll.Body.String(), `"status":"generating"`) {
		t.Fatalf("poll body missing generating status: %s", poll.Body)
	}

	// Flip ready directly via SQL — standing in for the render worker
	// (task 4), which is exercised separately in worker_test.go.
	s3Key := "tenants/" + tenant.String() + "/reports/" + created.ReportID.String() + ".pdf"
	if _, err := pool.Exec(context.Background(),
		`UPDATE report SET status='ready', s3_key=$2, generated_at=now() WHERE id=$1`,
		created.ReportID, s3Key,
	); err != nil {
		t.Fatalf("flip ready: %v", err)
	}

	ready := api.Get("/api/v1/staff/reports/"+created.ReportID.String(), bearer)
	if ready.Code != http.StatusOK {
		t.Fatalf("get ready: got %d; body %s", ready.Code, ready.Body)
	}
	body := ready.Body.String()
	for _, want := range []string{`"status":"ready"`, `"download_url":"https://s3.example/GET/` + s3Key, `"url_expires_at"`} {
		if !strings.Contains(body, want) {
			t.Errorf("ready body missing %q; body: %s", want, body)
		}
	}

	// History list includes the report.
	list := api.Get("/api/v1/staff/reports", bearer)
	if list.Code != http.StatusOK {
		t.Fatalf("list: got %d; body %s", list.Code, list.Body)
	}
	if !strings.Contains(list.Body.String(), created.ReportID.String()) {
		t.Fatalf("list missing report id; body: %s", list.Body)
	}
}

func TestCreateReportInvalidPeriod(t *testing.T) {
	pool := testdb.New(t)
	tenant, admin := seedTenantAndAdmin(t, pool)
	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, admin, auth.RoleAdmin)

	cases := []struct {
		name string
		body map[string]any
	}{
		{"from after to", map[string]any{"period_from": "2026-06-30", "period_to": "2026-06-01"}},
		// 2026-01-01 to 2026-04-04 is a 93-day span (31+28+31+3), one day
		// over the 92-day cap.
		{"93-day span", map[string]any{"period_from": "2026-01-01", "period_to": "2026-04-04"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := api.Post("/api/v1/staff/reports", bearer, c.body)
			if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid-period") {
				t.Fatalf("got %d body %s, want 422 invalid-period", rec.Code, rec.Body)
			}
		})
	}
}

func TestCreateReportInvalidDate(t *testing.T) {
	pool := testdb.New(t)
	tenant, admin := seedTenantAndAdmin(t, pool)
	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, admin, auth.RoleAdmin)

	rec := api.Post("/api/v1/staff/reports", bearer, map[string]any{
		"period_from": "not-a-date",
		"period_to":   "2026-06-30",
	})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid-date") {
		t.Fatalf("got %d body %s, want 422 invalid-date", rec.Code, rec.Body)
	}
}

// TestCreateReportForeignContractIsInvalidReference proves a contract_id
// belonging to another tenant is rejected exactly like an unknown one — the
// GetContractName lookup is tenant-scoped, so cross-tenant leakage isn't
// even distinguishable from "doesn't exist".
func TestCreateReportForeignContractIsInvalidReference(t *testing.T) {
	pool := testdb.New(t)
	tenant, admin := seedTenantAndAdmin(t, pool)
	otherTenant, _ := seedTenantAndAdmin(t, pool)
	contractID := uuid.New()
	if _, err := pool.Exec(context.Background(),
		`INSERT INTO contract (id, tenant_id, name) VALUES ($1,$2,'Other Tenant Contract')`,
		contractID, otherTenant,
	); err != nil {
		t.Fatalf("seed foreign contract: %v", err)
	}

	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, admin, auth.RoleAdmin)
	rec := api.Post("/api/v1/staff/reports", bearer, map[string]any{
		"contract_id": contractID.String(),
		"period_from": "2026-06-01",
		"period_to":   "2026-06-30",
	})
	if rec.Code != http.StatusUnprocessableEntity || !strings.Contains(rec.Body.String(), "invalid-reference") {
		t.Fatalf("got %d body %s, want 422 invalid-reference", rec.Code, rec.Body)
	}
}

func TestGetReportNotFound(t *testing.T) {
	pool := testdb.New(t)
	tenant, admin := seedTenantAndAdmin(t, pool)
	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, admin, auth.RoleAdmin)

	rec := api.Get("/api/v1/staff/reports/"+uuid.NewString(), bearer)
	if rec.Code != http.StatusNotFound || !strings.Contains(rec.Body.String(), "report-not-found") {
		t.Fatalf("got %d body %s, want 404 report-not-found", rec.Code, rec.Body)
	}
}

// TestCreateReportRejectsWorkerRole proves the path-prefix role gate
// (auth.Authorize, /api/v1/staff/*) already rejects a worker-role token
// before the handler ever runs — no extra role check needed in http.go.
func TestCreateReportRejectsWorkerRole(t *testing.T) {
	pool := testdb.New(t)
	tenant, _ := seedTenantAndAdmin(t, pool)
	api := buildReportAPI(t, pool)
	bearer := "Authorization: " + bearerFor(t, tenant, uuid.New(), auth.RoleWorker)

	rec := api.Post("/api/v1/staff/reports", bearer, map[string]any{
		"period_from": "2026-06-01",
		"period_to":   "2026-06-30",
	})
	if rec.Code != http.StatusForbidden {
		t.Fatalf("got %d body %s, want 403", rec.Code, rec.Body)
	}
}
