package repositories

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// ProviderKeyStorage is the interface satisfied by ProviderKeyRepository.
type ProviderKeyStorage interface {
	Upsert(ctx context.Context, provider, encryptedKey string) error
	HasKey(ctx context.Context, provider string) (bool, error)
	GetKey(ctx context.Context, provider string) (*models.ProviderAPIKey, error)
	List(ctx context.Context) ([]models.ProviderKeyInfo, error)
	Delete(ctx context.Context, provider string) error
}

// ProviderKeyRepository handles encrypted upstream provider credential data
// access. Single-tenant: one credential per provider.
type ProviderKeyRepository struct {
	db *sqlx.DB
}

// NewProviderKeyRepository constructs a ProviderKeyRepository backed by the given database.
func NewProviderKeyRepository(db *sqlx.DB) *ProviderKeyRepository {
	return &ProviderKeyRepository{db: db}
}

// Upsert stores the encrypted credential for a provider, replacing any existing
// one (keyed by the unique provider column).
func (r *ProviderKeyRepository) Upsert(ctx context.Context, provider, encryptedKey string) error {
	_, err := r.db.ExecContext(ctx, `
		INSERT INTO provider_api_keys (provider, encrypted_key, updated_at)
		VALUES ($1, $2, now())
		ON CONFLICT (provider)
		DO UPDATE SET encrypted_key = $2, updated_at = now()`,
		provider, encryptedKey)
	if err != nil {
		return fmt.Errorf("upsert provider key: %w", err)
	}
	return nil
}

// HasKey reports whether a credential is stored for the provider. Used by the
// provider router to hard-filter candidate endpoints to those it can actually
// authenticate before selecting one.
func (r *ProviderKeyRepository) HasKey(ctx context.Context, provider string) (bool, error) {
	var exists bool
	if err := r.db.GetContext(ctx, &exists,
		`SELECT EXISTS(SELECT 1 FROM provider_api_keys WHERE provider = $1)`,
		provider); err != nil {
		return false, fmt.Errorf("check provider key: %w", err)
	}
	return exists, nil
}

// GetKey returns the stored credential (including ciphertext) for a provider.
// Returns ErrProviderKeyNotFound when none is stored.
func (r *ProviderKeyRepository) GetKey(ctx context.Context, provider string) (*models.ProviderAPIKey, error) {
	var key models.ProviderAPIKey
	if err := r.db.GetContext(ctx, &key,
		`SELECT id, provider, encrypted_key, created_at, updated_at FROM provider_api_keys WHERE provider = $1`,
		provider); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrProviderKeyNotFound
		}
		return nil, fmt.Errorf("get provider key: %w", err)
	}
	return &key, nil
}

// List returns the safe (key-free) view of all stored provider credentials.
func (r *ProviderKeyRepository) List(ctx context.Context) ([]models.ProviderKeyInfo, error) {
	var keys []models.ProviderKeyInfo
	if err := r.db.SelectContext(ctx, &keys,
		`SELECT provider, created_at FROM provider_api_keys ORDER BY provider`); err != nil {
		return nil, fmt.Errorf("list provider keys: %w", err)
	}
	return keys, nil
}

// Delete removes the stored credential for a provider. Returns
// ErrProviderKeyNotFound when none was stored.
func (r *ProviderKeyRepository) Delete(ctx context.Context, provider string) error {
	result, err := r.db.ExecContext(ctx,
		`DELETE FROM provider_api_keys WHERE provider = $1`, provider)
	if err != nil {
		return fmt.Errorf("delete provider key: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete provider key rows affected: %w", err)
	}
	if n == 0 {
		return ErrProviderKeyNotFound
	}
	return nil
}
