package services

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// backfillBatchSize bounds each retro-index / cleanup batch when a key is
// (de)activated, so the work never locks the table for long.
const backfillBatchSize = 1000

// MetadataService manages discovery and activation of request-metadata keys. It owns
// the orchestration of (de)activation with the asynchronous backfill/cleanup of
// existing rows so that past usage becomes (or stops being) queryable.
type MetadataService struct {
	repo repositories.MetadataKeyStorage
}

// NewMetadataService constructs a MetadataService.
func NewMetadataService(repo repositories.MetadataKeyStorage) *MetadataService {
	return &MetadataService{repo: repo}
}

// ListKeys returns every discovered metadata key with its approximate cardinality and
// whether it is currently indexed.
func (s *MetadataService) ListKeys(ctx context.Context) ([]*models.MetadataKey, error) {
	return s.repo.ListMetadataKeys(ctx)
}

// Activate marks a metadata key indexed for an API key and backfills existing rows
// asynchronously so past usage becomes queryable too. It returns
// repositories.ErrMetadataKeyNotFound when the key is unknown for that API key.
func (s *MetadataService) Activate(ctx context.Context, apiKeyID uuid.UUID, keyName string) error {
	if err := s.repo.ActivateMetadataKey(ctx, apiKeyID, keyName); err != nil {
		return err
	}
	go s.backfill(apiKeyID, keyName)
	return nil
}

// Deactivate stops indexing a metadata key for an API key and removes it from existing
// indexed_metadata rows asynchronously. It returns
// repositories.ErrMetadataKeyNotFound when the key is unknown for that API key.
func (s *MetadataService) Deactivate(ctx context.Context, apiKeyID uuid.UUID, keyName string) error {
	if err := s.repo.DeactivateMetadataKey(ctx, apiKeyID, keyName); err != nil {
		return err
	}
	go s.cleanup(apiKeyID, keyName)
	return nil
}

// backfill runs the retro-indexing in the background, detached from the request
// context so it completes even after the HTTP response is sent.
func (s *MetadataService) backfill(apiKeyID uuid.UUID, keyName string) {
	if _, err := s.repo.BackfillIndexedMetadata(context.Background(), apiKeyID, keyName, backfillBatchSize); err != nil {
		slog.Error("metadata backfill failed", "error", err, "api_key_id", apiKeyID, "key", keyName)
	}
}

// cleanup removes indexed rows in the background, detached from the request context.
func (s *MetadataService) cleanup(apiKeyID uuid.UUID, keyName string) {
	if _, err := s.repo.CleanupIndexedMetadata(context.Background(), apiKeyID, keyName, backfillBatchSize); err != nil {
		slog.Error("metadata cleanup failed", "error", err, "api_key_id", apiKeyID, "key", keyName)
	}
}
