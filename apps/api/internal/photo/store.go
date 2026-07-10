// Package photo exposes the worker two-phase photo upload: presign (issue a
// direct-to-S3 PUT URL) and confirm (verify the object landed, then stamp the
// row). The API never streams photo bytes.
package photo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
)

// presignTTL is the lifetime of a presigned PUT (spec §7: 15 min PUT).
const presignTTL = 15 * time.Minute

// presignGetTTL is the lifetime of a presigned GET (spec §7: 5 min GET).
const presignGetTTL = 5 * time.Minute

// maxObjectBytes bounds Get's read: a photo well beyond this size can only
// mean a corrupt or unexpected object, never legitimate evidence. Get must
// fail loudly rather than silently truncate — a truncated photo would
// corrupt report evidence (AGENTS.md: evidence is sacred).
const maxObjectBytes = 50 << 20

// ObjectStore is the slice of object-storage behaviour the photo handlers need.
// Injecting it keeps the handlers testable without a live S3.
type ObjectStore interface {
	PresignPut(ctx context.Context, key, contentType string) (url string, expiresAt time.Time, err error)
	Exists(ctx context.Context, key string) (bool, error)
	PresignGet(ctx context.Context, key string) (url string, expiresAt time.Time, err error)
	// Get downloads an object's full bytes. Used by the report renderer
	// (task 3) to fetch original photos for downscaling — the API never
	// otherwise streams file bytes (AGENTS.md stack table), so this stays
	// off the request path and is only called from the async report worker.
	Get(ctx context.Context, key string) ([]byte, error)
	// Put uploads bytes directly, used by the report worker to store the
	// generated PDF.
	Put(ctx context.Context, key, contentType string, body []byte) error
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

func (s *S3Store) PresignGet(ctx context.Context, key string) (string, time.Time, error) {
	req, err := s3.NewPresignClient(s.client).PresignGetObject(ctx, &s3.GetObjectInput{
		Bucket: &s.bucket,
		Key:    &key,
	}, s3.WithPresignExpires(presignGetTTL))
	if err != nil {
		return "", time.Time{}, fmt.Errorf("presigning get %s: %w", key, err)
	}
	return req.URL, time.Now().Add(presignGetTTL), nil
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

// Get downloads an object's full bytes from S3. The read is bounded at
// maxObjectBytes: an object beyond that limit is never truncated (a
// truncated photo would silently corrupt report evidence) — Get fails
// loudly instead, naming the key and the limit.
func (s *S3Store) Get(ctx context.Context, key string) ([]byte, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &key})
	if err != nil {
		return nil, fmt.Errorf("get object %s: %w", key, err)
	}
	defer func() { _ = out.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxObjectBytes+1))
	if err != nil {
		return nil, fmt.Errorf("reading object %s: %w", key, err)
	}
	if len(data) > maxObjectBytes {
		return nil, fmt.Errorf("reading object %s: exceeds %d byte limit", key, maxObjectBytes)
	}
	return data, nil
}

// Put uploads body to S3 under key with contentType.
func (s *S3Store) Put(ctx context.Context, key, contentType string, body []byte) error {
	_, err := s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &key,
		Body:        bytes.NewReader(body),
		ContentType: &contentType,
	})
	if err != nil {
		return fmt.Errorf("put object %s: %w", key, err)
	}
	return nil
}
