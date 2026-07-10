package report

// http.go exposes the staff-facing report endpoints: async enqueue,
// poll-by-id, and history list. The render worker (worker.go) drains what
// these handlers insert; this file contains no rendering or S3 logic of its
// own beyond presigning a GET for an already-rendered PDF.

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
	"sdano.app/api/internal/photo"
)

// apiDateLayout is the wire format for period_from/period_to
// (YYYY-MM-DD). Named distinctly from data.go's dateLayout (DD.MM.YYYY,
// used for on-page display in the rendered report) so the two never get
// confused despite living in the same package.
const apiDateLayout = "2006-01-02"

// maxPeriodSpan bounds how wide a report's date range may be: a report is a
// single synchronous-ish render job (headless Chrome, one HTTP request's
// worth of work on the worker side), and an unbounded span would let a
// request enqueue an arbitrarily expensive render. 92 days covers a full
// calendar quarter, the longest period a municipal contract report
// realistically spans.
const maxPeriodSpan = 92 * 24 * time.Hour

// maxPendingReports caps how many 'generating' rows one tenant may hold in
// the queue at once. The render worker is a single FIFO consumer across all
// tenants, so without a cap one tenant's retry loop could starve everyone
// else's reports. It is a soft cap (two concurrent creates may briefly
// overshoot by one — the count and the insert are separate statements), which
// is fine: the point is bounding queue depth, not enforcing an invariant.
const maxPendingReports = 5

func problem(status int, slug, detail string) *huma.ErrorModel {
	return &huma.ErrorModel{Type: slug, Title: http.StatusText(status), Status: status, Detail: detail}
}

// Register wires the staff-facing report endpoints onto api.
func Register(api huma.API, pool *pgxpool.Pool, store photo.ObjectStore) {
	q := db.New(pool)
	registerCreateReport(api, q)
	registerGetReport(api, q, store)
	registerListReports(api, q)
}

// reportView is the shape returned by both getStaffReport and
// listStaffReports. FailureReason/DownloadURL/URLExpiresAt are pointers with
// omitempty so they vanish from the JSON entirely (not an empty string) when
// not applicable — failure_reason only appears for a failed report,
// download_url/url_expires_at only for a ready one with an S3 key.
type reportView struct {
	ID            uuid.UUID  `json:"id"`
	ContractID    *uuid.UUID `json:"contract_id,omitempty"`
	Status        string     `json:"status"`
	PeriodFrom    string     `json:"period_from"`
	PeriodTo      string     `json:"period_to"`
	CreatedAt     time.Time  `json:"created_at"`
	FailureReason *string    `json:"failure_reason,omitempty"`
	DownloadURL   *string    `json:"download_url,omitempty"`
	URLExpiresAt  *time.Time `json:"url_expires_at,omitempty"`
}

func nullUUIDPtr(v uuid.NullUUID) *uuid.UUID {
	if !v.Valid {
		return nil
	}
	id := v.UUID
	return &id
}

// parsePeriod parses and validates a report's requested date range: both
// dates must parse as YYYY-MM-DD (invalid-date), and the range must be
// non-inverted and no wider than maxPeriodSpan (invalid-period).
func parsePeriod(fromStr, toStr string) (from, to time.Time, problemErr *huma.ErrorModel) {
	from, err := time.Parse(apiDateLayout, fromStr)
	if err != nil {
		return time.Time{}, time.Time{}, problem(http.StatusUnprocessableEntity, "invalid-date", "period_from must be YYYY-MM-DD")
	}
	to, err = time.Parse(apiDateLayout, toStr)
	if err != nil {
		return time.Time{}, time.Time{}, problem(http.StatusUnprocessableEntity, "invalid-date", "period_to must be YYYY-MM-DD")
	}
	if to.Before(from) || to.Sub(from) > maxPeriodSpan {
		return time.Time{}, time.Time{}, problem(http.StatusUnprocessableEntity, "invalid-period", "period_to must be on or after period_from, spanning at most 92 days")
	}
	return from, to, nil
}

type createReportInput struct {
	Body struct {
		ContractID *string `json:"contract_id,omitempty" format:"uuid"`
		PeriodFrom string  `json:"period_from" example:"2026-06-01" doc:"YYYY-MM-DD"`
		PeriodTo   string  `json:"period_to" example:"2026-06-30" doc:"YYYY-MM-DD"`
	}
}

type createReportOutput struct {
	Body struct {
		ReportID uuid.UUID `json:"report_id"`
		Status   string    `json:"status"`
	}
}

// registerCreateReport wires POST /api/v1/staff/reports: enqueues a report
// row (status 'generating', the render queue itself per docs/06) that the
// worker picks up asynchronously — generation is a headless-Chrome render
// that can take tens of seconds, so this endpoint never blocks on it.
// Carries auth.SuspendedWritable(): a suspended tenant may still generate
// reports for past periods (docs/12) even though it can't create new work.
func registerCreateReport(api huma.API, q *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID:   "createStaffReport",
		Method:        http.MethodPost,
		Path:          "/api/v1/staff/reports",
		Summary:       "Enqueue a report for async generation",
		Tags:          []string{"staff"},
		Metadata:      auth.SuspendedWritable(),
		DefaultStatus: http.StatusAccepted,
	}, func(ctx context.Context, in *createReportInput) (*createReportOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		from, to, perr := parsePeriod(in.Body.PeriodFrom, in.Body.PeriodTo)
		if perr != nil {
			return nil, perr
		}
		pending, err := q.CountGeneratingReports(ctx, p.TenantID)
		if err != nil {
			return nil, fmt.Errorf("counting pending reports for tenant %s: %w", p.TenantID, err)
		}
		if pending >= maxPendingReports {
			return nil, problem(http.StatusTooManyRequests, "report-queue-full",
				fmt.Sprintf("this tenant already has %d reports generating; wait for one to finish before enqueueing another", pending))
		}
		var contractID uuid.NullUUID
		if in.Body.ContractID != nil {
			cid, err := uuid.Parse(*in.Body.ContractID)
			if err != nil {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid contract_id")
			}
			if _, err := q.GetContractName(ctx, db.GetContractNameParams{ID: cid, TenantID: p.TenantID}); errors.Is(err, pgx.ErrNoRows) {
				return nil, problem(http.StatusUnprocessableEntity, "invalid-reference", "contract_id is unknown for this tenant")
			} else if err != nil {
				return nil, fmt.Errorf("validating contract %s: %w", cid, err)
			}
			contractID = uuid.NullUUID{UUID: cid, Valid: true}
		}
		row, err := q.InsertReport(ctx, db.InsertReportParams{
			TenantID:    p.TenantID,
			ContractID:  contractID,
			PeriodFrom:  pgtype.Date{Time: from, Valid: true},
			PeriodTo:    pgtype.Date{Time: to, Valid: true},
			GeneratedBy: uuid.NullUUID{UUID: p.UserID, Valid: true},
		})
		if err != nil {
			return nil, fmt.Errorf("inserting report: %w", err)
		}
		out := &createReportOutput{}
		out.Body.ReportID = row.ID
		out.Body.Status = string(row.Status)
		return out, nil
	})
}

type getReportInput struct {
	ID string `path:"id"`
}

type getReportOutput struct {
	Body reportView
}

// registerGetReport wires GET /api/v1/staff/reports/{id}: the poll target
// for the async create above. download_url is only populated once the
// render worker has flipped status to 'ready' and stamped s3_key — it is
// absent (not an empty string) while generating and after a failure.
func registerGetReport(api huma.API, q *db.Queries, store photo.ObjectStore) {
	huma.Register(api, huma.Operation{
		OperationID: "getStaffReport",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/reports/{id}",
		Summary:     "Poll a report's generation status",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, in *getReportInput) (*getReportOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		id, err := uuid.Parse(in.ID)
		if err != nil {
			return nil, problem(http.StatusUnprocessableEntity, "invalid-uuid", "invalid report id")
		}
		row, err := q.GetReport(ctx, db.GetReportParams{ID: id, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "report-not-found", "report not found")
		}
		if err != nil {
			return nil, fmt.Errorf("loading report %s: %w", id, err)
		}
		view := reportView{
			ID:         row.ID,
			ContractID: nullUUIDPtr(row.ContractID),
			Status:     string(row.Status),
			PeriodFrom: row.PeriodFrom.Time.Format(apiDateLayout),
			PeriodTo:   row.PeriodTo.Time.Format(apiDateLayout),
			CreatedAt:  row.CreatedAt.Time,
		}
		if row.Status == db.ReportStatusFailed && row.FailureReason != nil {
			view.FailureReason = row.FailureReason
		}
		if row.Status == db.ReportStatusReady && row.S3Key != nil {
			url, expires, err := store.PresignGet(ctx, *row.S3Key)
			if err != nil {
				return nil, fmt.Errorf("presigning report %s: %w", id, err)
			}
			view.DownloadURL = &url
			view.URLExpiresAt = &expires
		}
		return &getReportOutput{Body: view}, nil
	})
}

type listReportsOutput struct {
	Body struct {
		Reports []reportView `json:"reports"`
	}
}

// registerListReports wires GET /api/v1/staff/reports: history, most recent
// first (ListReports already orders/caps at 100). It never presigns per
// row — download_url only ever appears from the single-report GET above, so
// listing a long history stays a single cheap SQL query.
func registerListReports(api huma.API, q *db.Queries) {
	huma.Register(api, huma.Operation{
		OperationID: "listStaffReports",
		Method:      http.MethodGet,
		Path:        "/api/v1/staff/reports",
		Summary:     "List generated reports",
		Tags:        []string{"staff"},
	}, func(ctx context.Context, _ *struct{}) (*listReportsOutput, error) {
		p, ok := auth.PrincipalFrom(ctx)
		if !ok {
			return nil, huma.Error401Unauthorized("authentication required")
		}
		rows, err := q.ListReports(ctx, p.TenantID)
		if err != nil {
			return nil, fmt.Errorf("listing reports for tenant %s: %w", p.TenantID, err)
		}
		out := &listReportsOutput{}
		out.Body.Reports = make([]reportView, 0, len(rows))
		for _, r := range rows {
			out.Body.Reports = append(out.Body.Reports, reportView{
				ID:         r.ID,
				ContractID: nullUUIDPtr(r.ContractID),
				Status:     string(r.Status),
				PeriodFrom: r.PeriodFrom.Time.Format(apiDateLayout),
				PeriodTo:   r.PeriodTo.Time.Format(apiDateLayout),
				CreatedAt:  r.CreatedAt.Time,
			})
		}
		return out, nil
	})
}
