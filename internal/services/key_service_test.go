package services

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// fakeAPIKeyStore implements repositories.APIKeyStorage; only the methods the service
// calls are overridden, the rest are promoted from the embedded nil interface.
type fakeAPIKeyStore struct {
	repositories.APIKeyStorage
	gotHash   string
	created   *models.APIKey
	revokeErr error
	revokedID uuid.UUID
}

func (f *fakeAPIKeyStore) CreateAPIKey(_ context.Context, keyHash string, input *models.CreateAPIKeyInput) (*models.APIKey, error) {
	f.gotHash = keyHash
	f.created = &models.APIKey{ID: uuid.New(), Name: input.Name, KeyHash: keyHash}
	return f.created, nil
}

func (f *fakeAPIKeyStore) RevokeAPIKey(_ context.Context, id uuid.UUID) error {
	f.revokedID = id
	return f.revokeErr
}

func TestKeyService_CreateKey(t *testing.T) {
	store := &fakeAPIKeyStore{}
	svc := NewKeyService(store)

	key, plaintext, err := svc.CreateKey(context.Background(), &models.CreateAPIKeyInput{Name: "ci"})
	if err != nil {
		t.Fatalf("CreateKey: %v", err)
	}
	if plaintext == "" {
		t.Fatal("expected a non-empty one-time plaintext")
	}
	if store.gotHash == "" {
		t.Fatal("expected the hash (not the plaintext) to be persisted")
	}
	if store.gotHash == plaintext {
		t.Fatal("stored value must be the hash, not the plaintext")
	}
	if key == nil || key.Name != "ci" {
		t.Fatalf("unexpected key returned: %+v", key)
	}
}

func TestKeyService_RevokeKey_PropagatesNotFound(t *testing.T) {
	store := &fakeAPIKeyStore{revokeErr: repositories.ErrAPIKeyNotFound}
	svc := NewKeyService(store)

	id := uuid.New()
	err := svc.RevokeKey(context.Background(), id)
	if err != repositories.ErrAPIKeyNotFound {
		t.Fatalf("expected ErrAPIKeyNotFound to bubble, got %v", err)
	}
	if store.revokedID != id {
		t.Fatalf("expected repo to receive id %s, got %s", id, store.revokedID)
	}
}
