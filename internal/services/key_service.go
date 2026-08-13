package services

import (
	"context"
	"fmt"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/auth"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// KeyService manages gateway API keys. It owns key-material generation: the plaintext
// is produced here and returned once, while only the hash is persisted.
type KeyService struct {
	keys repositories.APIKeyStorage
}

// NewKeyService constructs a KeyService.
func NewKeyService(keys repositories.APIKeyStorage) *KeyService {
	return &KeyService{keys: keys}
}

// CreateKey generates a new API key, persists its hash, and returns the created key
// together with the one-time plaintext (never stored). The caller must surface the
// plaintext to the user exactly once.
func (s *KeyService) CreateKey(ctx context.Context, input *models.CreateAPIKeyInput) (*models.APIKey, string, error) {
	plaintext, hash, err := auth.GenerateAPIKey()
	if err != nil {
		return nil, "", fmt.Errorf("generate api key: %w", err)
	}

	key, err := s.keys.CreateAPIKey(ctx, hash, input)
	if err != nil {
		return nil, "", fmt.Errorf("create api key: %w", err)
	}

	return key, plaintext, nil
}

// ListKeys returns all API keys.
func (s *KeyService) ListKeys(ctx context.Context) ([]*models.APIKey, error) {
	return s.keys.ListAPIKeys(ctx)
}

// RevokeKey revokes the API key with the given ID. It returns
// repositories.ErrAPIKeyNotFound when no such key exists.
func (s *KeyService) RevokeKey(ctx context.Context, id uuid.UUID) error {
	return s.keys.RevokeAPIKey(ctx, id)
}
