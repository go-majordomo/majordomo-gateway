package storage

import (
	"context"
	"fmt"
	"io"

	gcs "cloud.google.com/go/storage"
)

// GCSStore stores bodies in a single GCS bucket. Credentials come from the
// application default chain (GOOGLE_APPLICATION_CREDENTIALS or the instance's
// service account).
type GCSStore struct {
	client *gcs.Client
	bucket string
}

// GCSConfig configures the GCS body store.
type GCSConfig struct {
	Bucket string
}

// NewGCSStore builds a GCS-backed Store.
func NewGCSStore(ctx context.Context, cfg GCSConfig) (*GCSStore, error) {
	if cfg.Bucket == "" {
		return nil, fmt.Errorf("gcs body storage requires a bucket")
	}
	client, err := gcs.NewClient(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs client: %w", err)
	}
	return &GCSStore{client: client, bucket: cfg.Bucket}, nil
}

func (g *GCSStore) Upload(ctx context.Context, key string, p *BodyPayload) error {
	data, err := encode(p)
	if err != nil {
		return err
	}
	obj := g.client.Bucket(g.bucket).Object(key)
	w := obj.NewWriter(ctx)
	w.ContentType = "application/json"
	w.ContentEncoding = "gzip"
	if _, err := w.Write(data); err != nil {
		w.Close()
		return fmt.Errorf("gcs write %s/%s: %w", g.bucket, key, err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("gcs close %s/%s: %w", g.bucket, key, err)
	}
	return nil
}

func (g *GCSStore) Download(ctx context.Context, key string) (*BodyContent, error) {
	obj := g.client.Bucket(g.bucket).Object(key)
	r, err := obj.NewReader(ctx)
	if err != nil {
		return nil, fmt.Errorf("gcs read %s/%s: %w", g.bucket, key, err)
	}
	defer r.Close()

	compressed, err := io.ReadAll(r)
	if err != nil {
		return nil, fmt.Errorf("reading gcs body: %w", err)
	}
	return decode(compressed)
}
