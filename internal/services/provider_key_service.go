package services

import (
	"context"
	"fmt"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/provider"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/secrets"
)

// ProviderKeyService manages stored upstream provider credentials. It owns the
// domain rule that a provider must be a known credential provider and the server-side
// encryption of key material before it is persisted.
type ProviderKeyService struct {
	repo    repositories.ProviderKeyStorage
	secrets secrets.SecretStore
}

// NewProviderKeyService constructs a ProviderKeyService.
func NewProviderKeyService(repo repositories.ProviderKeyStorage, secrets secrets.SecretStore) *ProviderKeyService {
	return &ProviderKeyService{repo: repo, secrets: secrets}
}

// UpsertKey validates the provider, encrypts the plaintext key, and stores it,
// replacing any existing credential for that provider. It returns ErrUnknownProvider
// when providerName is not a known credential provider.
func (s *ProviderKeyService) UpsertKey(ctx context.Context, providerName, plaintextKey string) error {
	if !provider.IsCredentialProvider(providerName) {
		return fmt.Errorf("%q: %w", providerName, ErrUnknownProvider)
	}

	encrypted, err := s.secrets.Encrypt(plaintextKey)
	if err != nil {
		return fmt.Errorf("encrypt provider key: %w", err)
	}
	if err := s.repo.Upsert(ctx, providerName, encrypted); err != nil {
		return fmt.Errorf("store provider key: %w", err)
	}
	return nil
}

// ListKeys returns metadata for every stored provider credential (never the key
// material itself).
func (s *ProviderKeyService) ListKeys(ctx context.Context) ([]models.ProviderKeyInfo, error) {
	return s.repo.List(ctx)
}

// DeleteKey removes the stored credential for a provider. It returns
// repositories.ErrProviderKeyNotFound when no credential exists for that provider.
func (s *ProviderKeyService) DeleteKey(ctx context.Context, providerName string) error {
	return s.repo.Delete(ctx, providerName)
}
