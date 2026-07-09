package app

import (
	"context"
	"fmt"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/jackc/pgx/v5/pgxpool"

	"sdano.app/api/internal/config"
)

func NewPool(ctx context.Context, cfg config.Config) (*pgxpool.Pool, error) {
	pool, err := pgxpool.New(ctx, cfg.DatabaseURL)
	if err != nil {
		return nil, fmt.Errorf("creating pgx pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		return nil, fmt.Errorf("pinging database: %w", err)
	}
	return pool, nil
}

// NewS3 builds an S3 client from cfg. Deviates from the task brief, which
// discards the LoadDefaultConfig error (awsCfg, _ := ...): errcheck (a
// standard golangci-lint linter) flags unchecked errors, so this returns
// the error to the caller instead.
func NewS3(cfg config.Config) (*s3.Client, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(),
		awsconfig.WithRegion(cfg.S3Region),
		awsconfig.WithCredentialsProvider(
			credentials.NewStaticCredentialsProvider(cfg.S3AccessKey, cfg.S3SecretKey, "")),
	)
	if err != nil {
		return nil, fmt.Errorf("loading aws config: %w", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = &cfg.S3Endpoint
		o.UsePathStyle = cfg.S3UsePathStyle
	}), nil
}

func DBCheck(pool *pgxpool.Pool) HealthCheck {
	return HealthCheck{Name: "postgres", Ping: pool.Ping}
}

func S3Check(client *s3.Client, bucket string) HealthCheck {
	return HealthCheck{Name: "s3", Ping: func(ctx context.Context) error {
		_, err := client.HeadBucket(ctx, &s3.HeadBucketInput{Bucket: &bucket})
		if err != nil {
			return fmt.Errorf("head bucket %s: %w", bucket, err)
		}
		return nil
	}}
}
