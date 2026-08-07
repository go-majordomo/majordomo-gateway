package models

import (
	"time"

	"github.com/google/uuid"
)

// MetadataKey is a metadata key discovered on logged requests. Keys are recorded
// automatically; only keys an operator has activated are copied into indexed_metadata
// (and thus become queryable). ApproxCardinality is a HyperLogLog estimate of the
// number of distinct values seen, so high-dimension keys (e.g. user_id) can be spotted
// and left un-indexed.
type MetadataKey struct {
	MajordomoAPIKeyID uuid.UUID  `json:"majordomo_api_key_id" db:"majordomo_api_key_id"`
	APIKeyName        string     `json:"api_key_name" db:"api_key_name"`
	KeyName           string     `json:"key_name" db:"key_name"`
	DisplayName       *string    `json:"display_name,omitempty" db:"display_name"`
	KeyType           string     `json:"key_type" db:"key_type"`
	IsRequired        bool       `json:"is_required" db:"is_required"`
	IsActive          bool       `json:"is_active" db:"is_active"`
	ActivatedAt       *time.Time `json:"activated_at,omitempty" db:"activated_at"`
	RequestCount      int64      `json:"request_count" db:"request_count"`
	LastSeenAt        *time.Time `json:"last_seen_at,omitempty" db:"last_seen_at"`
	ApproxCardinality int64      `json:"approx_cardinality" db:"approx_cardinality"`
	CreatedAt         time.Time  `json:"created_at" db:"created_at"`
}
