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
		if in.Body.ContentType != "image/jpeg" {
			return nil, problem(http.StatusUnprocessableEntity, "unsupported-content-type", "only image/jpeg is supported")
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
		// photo.id is a client-generated, globally-unique primary key (not
		// scoped per execution or tenant), and the insert above is an
		// idempotent ON CONFLICT (id) DO NOTHING — the intended replay case
		// is the *same* worker re-presigning the *same* photo for the *same*
		// execution. If id instead collided with a row already bound to a
		// different execution, the insert silently no-ops; without this
		// check we would still hand back a presigned URL, and confirm would
		// later stamp uploaded_at on the wrong execution's row (or overwrite
		// its uploaded object at the same S3 key) — silently corrupting
		// evidence. Defense in depth against a ~2^-122 collision, mirroring
		// the ExecutionID-conflict guard added for work_execution in the
		// PUT /worker/executions/{id} handler.
		got, err := q.GetPhoto(ctx, db.GetPhotoParams{ID: photoID, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusConflict, "photo-id-conflict", "photo id already in use")
		}
		if err != nil {
			return nil, fmt.Errorf("verifying photo row: %w", err)
		}
		if !got.ExecutionID.Valid || got.ExecutionID.UUID != execID {
			return nil, problem(http.StatusConflict, "photo-id-conflict", "photo id already in use")
		}
		// Same guard, but for kind: InsertPhotoPresign's ON CONFLICT DO
		// NOTHING also no-ops when the id collides on the SAME execution but
		// with a DIFFERENT kind, so we would otherwise hand back a fresh
		// presigned URL while the row silently kept its original kind,
		// mislabeling the evidence that eventually lands at this id.
		if string(got.Kind) != in.Body.Kind {
			return nil, problem(http.StatusConflict, "photo-id-conflict", "photo id already in use with a different kind")
		}
		// Evidence immutability: once a photo has been confirmed (uploaded_at
		// stamped), the uploaded bytes must not be replaceable via a fresh
		// presigned URL. A re-presign of an UNCONFIRMED photo (uploaded_at
		// still null) is still allowed and intentional — that's the
		// resumability path for a client retrying an interrupted upload
		// before confirm.
		if got.UploadedAt.Valid {
			return nil, problem(http.StatusConflict, "photo-already-uploaded", "photo already confirmed; cannot re-presign")
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
		TakenAt *time.Time `json:"taken_at,omitempty"`
		Lat     *float64   `json:"lat,omitempty"`
		Lon     *float64   `json:"lon,omitempty"`
	}
}

type photoView struct {
	ID         uuid.UUID  `json:"id"`
	Kind       string     `json:"kind"`
	TakenAt    *time.Time `json:"taken_at,omitempty"`
	Lat        *float64   `json:"lat,omitempty"`
	Lon        *float64   `json:"lon,omitempty"`
	UploadedAt *time.Time `json:"uploaded_at,omitempty"`
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
		if !ph.ExecutionID.Valid {
			return nil, problem(http.StatusNotFound, "execution-not-found", "photo has no execution")
		}
		ex, err := q.GetExecutionForWorker(ctx, db.GetExecutionForWorkerParams{ID: ph.ExecutionID.UUID, TenantID: p.TenantID})
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, problem(http.StatusNotFound, "execution-not-found", "execution not found")
		}
		if err != nil {
			return nil, fmt.Errorf("loading execution: %w", err)
		}
		if ex.WorkerID != p.UserID {
			return nil, problem(http.StatusForbidden, "work-order-not-assigned", "photo belongs to another worker")
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
