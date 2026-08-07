package storage

import (
	"bytes"
	"context"
	"fmt"
	"io"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// S3Store stores bodies in a single S3 (or S3-compatible) bucket. Credentials come
// from the AWS SDK default chain (env AWS_*, shared config, or the instance role).
type S3Store struct {
	client *s3.Client
	bucket string
}

// S3Config configures the S3 body store. Endpoint is optional and set for
// S3-compatible providers (MinIO, Cloudflare R2, etc.).
type S3Config struct {
	Bucket   string
	Region   string
	Endpoint string
}

// NewS3Store builds an S3-backed Store.
func NewS3Store(ctx context.Context, cfg S3Config) (*S3Store, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("s3 body storage requires a bucket")
	}
	var opts []func(*awsconfig.LoadOptions) error
	if cfg.Region != "" {
		opts = append(opts, awsconfig.WithRegion(cfg.Region))
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx, opts...)
	if err != nil {
		return nil, fmt.Errorf("load aws config: %w", err)
	}

	var s3Opts []func(*s3.Options)
	if cfg.Endpoint != "" {
		s3Opts = append(s3Opts, func(o *s3.Options) {
			o.BaseEndpoint = aws.String(cfg.Endpoint)
			o.UsePathStyle = true
		})
	}
	return &S3Store{client: s3.NewFromConfig(awsCfg, s3Opts...), bucket: cfg.Bucket}, nil
}

func (s *S3Store) Upload(ctx context.Context, key string, p *BodyPayload) error {
	data, err := encode(p)
	if err != nil {
		return err
	}
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:          aws.String(s.bucket),
		Key:             aws.String(key),
		Body:            bytes.NewReader(data),
		ContentType:     aws.String("application/json"),
		ContentEncoding: aws.String("gzip"),
	})
	if err != nil {
		return fmt.Errorf("s3 PutObject %s/%s: %w", s.bucket, key, err)
	}
	return nil
}

func (s *S3Store) Download(ctx context.Context, key string) (*BodyContent, error) {
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{
		Bucket: aws.String(s.bucket),
		Key:    aws.String(key),
	})
	if err != nil {
		return nil, fmt.Errorf("s3 GetObject %s/%s: %w", s.bucket, key, err)
	}
	defer out.Body.Close()

	compressed, err := io.ReadAll(out.Body)
	if err != nil {
		return nil, fmt.Errorf("reading s3 body: %w", err)
	}
	return decode(compressed)
}
