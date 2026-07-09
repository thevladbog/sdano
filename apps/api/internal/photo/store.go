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
