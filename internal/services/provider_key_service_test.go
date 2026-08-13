package services

import (
	"context"
	"errors"
	"testing"

	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// fakeProviderKeyStore implements repositories.ProviderKeyStorage; only Upsert is
// exercised here.
type fakeProviderKeyStore struct {
	repositories.ProviderKeyStorage
	upsertCalled bool
	gotProvider  string
	gotEncrypted string
}

func (f *fakeProviderKeyStore) Upsert(_ context.Context, provider, encryptedKey string) error {
	f.upsertCalled = true
	f.gotProvider = provider
	f.gotEncrypted = encryptedKey
	return nil
}

// stubSecretStore implements secrets.SecretStore with a visible transformation.
type stubSecretStore struct{}

func (stubSecretStore) Encrypt(plaintext string) (string, error) { return "enc:" + plaintext, nil }
func (stubSecretStore) Decrypt(ciphertext string) (string, error) {
	return ciphertext, nil
}

func TestProviderKeyService_UpsertKey_UnknownProvider(t *testing.T) {
	store := &fakeProviderKeyStore{}
	svc := NewProviderKeyService(store, stubSecretStore{})

	err := svc.UpsertKey(context.Background(), "not-a-provider", "sk-123")
	if !errors.Is(err, ErrUnknownProvider) {
		t.Fatalf("expected ErrUnknownProvider, got %v", err)
	}
	if store.upsertCalled {
		t.Fatal("repo must not be called for an unknown provider")
	}
}

func TestProviderKeyService_UpsertKey_EncryptsBeforeStoring(t *testing.T) {
	store := &fakeProviderKeyStore{}
	svc := NewProviderKeyService(store, stubSecretStore{})

	if err := svc.UpsertKey(context.Background(), "openai", "sk-secret"); err != nil {
		t.Fatalf("UpsertKey: %v", err)
	}
	if !store.upsertCalled {
		t.Fatal("expected repo.Upsert to be called")
	}
	if store.gotProvider != "openai" {
		t.Fatalf("expected provider openai, got %q", store.gotProvider)
	}
	if store.gotEncrypted != "enc:sk-secret" {
		t.Fatalf("expected encrypted key to be stored, got %q", store.gotEncrypted)
	}
}
