package models

import (
	"time"

	"github.com/google/uuid"
)

// ProviderAPIKey is a stored, AES-256-GCM encrypted upstream provider credential.
// Single-tenant: one credential per provider. The plaintext key is never
// persisted; EncryptedKey holds ciphertext and is never serialized to JSON.
type ProviderAPIKey struct {
	ID           uuid.UUID `json:"id" db:"id"`
	Provider     string    `json:"provider" db:"provider"`
	EncryptedKey string    `json:"-" db:"encrypted_key"` // Never expose in JSON
	CreatedAt    time.Time `json:"created_at" db:"created_at"`
	UpdatedAt    time.Time `json:"updated_at" db:"updated_at"`
}

// ProviderKeyInfo is the safe list view of a stored provider credential — the
// provider and when it was added, never the key material.
type ProviderKeyInfo struct {
	Provider  string    `json:"provider" db:"provider"`
	CreatedAt time.Time `json:"created_at" db:"created_at"`
}
