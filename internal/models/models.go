package models

import (
	"time"

	"github.com/google/uuid"
)

// APIKey represents a gateway API key stored in the database. Keys are minted
// locally by the CLI/admin API; the plaintext is shown once at creation and only
// the SHA-256 hash is persisted.
type APIKey struct {
	ID                      uuid.UUID               `json:"id" db:"id"`
	KeyHash                 string                  `json:"-" db:"key_hash"` // Never expose in JSON
	Name                    string                  `json:"name" db:"name"`
	Description             *string                 `json:"description,omitempty" db:"description"`
	IsActive                bool                    `json:"is_active" db:"is_active"`
	CreatedAt               time.Time               `json:"created_at" db:"created_at"`
	RevokedAt               *time.Time              `json:"revoked_at,omitempty" db:"revoked_at"`
	LastUsedAt              *time.Time              `json:"last_used_at,omitempty" db:"last_used_at"`
	RequestCount            int64                   `json:"request_count" db:"request_count"`
	DeprecatedModelBehavior DeprecatedModelBehavior `json:"deprecated_model_behavior" db:"deprecated_model_behavior"`
}

// CreateAPIKeyInput contains fields for creating a new API key.
type CreateAPIKeyInput struct {
	Name        string
	Description *string
}

// UpdateAPIKeyInput contains fields for updating an API key.
type UpdateAPIKeyInput struct {
	Name                    *string
	Description             *string
	DeprecatedModelBehavior *DeprecatedModelBehavior
}

// DeprecatedModelBehavior controls how the gateway handles requests for deprecated models.
type DeprecatedModelBehavior string

const (
	// DeprecatedModelBehaviorPassthrough forwards the request as-is; the upstream
	// provider will reject it if the model has been fully retired.
	DeprecatedModelBehaviorPassthrough DeprecatedModelBehavior = "passthrough"

	// DeprecatedModelBehaviorRedirect silently substitutes the recommended
	// replacement before forwarding. The original model is recorded in the log.
	DeprecatedModelBehaviorRedirect DeprecatedModelBehavior = "redirect"

	// DeprecatedModelBehaviorWarn redirects like Redirect but also adds
	// X-Majordomo-Deprecated-Model and X-Majordomo-Deprecated-Replacement headers
	// to the response so the caller can detect and fix the usage.
	DeprecatedModelBehaviorWarn DeprecatedModelBehavior = "warn"
)

// APIKeyInfo contains resolved API key information for request processing.
type APIKeyInfo struct {
	ID                      uuid.UUID               // Database ID for FK reference
	Hash                    string                  // SHA256 hash of the key
	Alias                   *string                 // Optional alias (key name)
	DeprecatedModelBehavior DeprecatedModelBehavior // How to handle deprecated model requests
}

type UsageMetrics struct {
	Provider            string
	Model               string
	InputTokens         int
	OutputTokens        int
	CachedTokens        int
	CacheCreationTokens int
	// TTL breakdown of CacheCreationTokens (Anthropic only). The two sum to
	// CacheCreationTokens; when the provider omits the breakdown, the total is
	// attributed to the 5-minute bucket (the Anthropic default TTL).
	CacheCreation5mTokens int
	CacheCreation1hTokens int
	ResponseTime          time.Duration
}

type Cost struct {
	InputCost         float64
	CachedCost        float64
	CacheCreationCost float64
	OutputCost        float64
	TotalCost         float64
	ModelAliasFound   bool
}

// RequestLog is one proxied request as persisted to the llm_requests table.
type RequestLog struct {
	ID uuid.UUID `json:"id" db:"id"`

	// Gateway API key (validated, for attribution/tracking)
	MajordomoAPIKeyID *uuid.UUID `json:"majordomo_api_key_id,omitempty" db:"majordomo_api_key_id"`

	// Provider API key (hashed Authorization header — relayed, never stored)
	ProviderAPIKeyHash  *string `json:"provider_api_key_hash,omitempty" db:"provider_api_key_hash"`
	ProviderAPIKeyAlias *string `json:"provider_api_key_alias,omitempty" db:"provider_api_key_alias"`

	Provider      string `json:"provider" db:"provider"`
	Model         string `json:"model" db:"model"`
	RequestPath   string `json:"request_path" db:"request_path"`
	RequestMethod string `json:"request_method" db:"request_method"`

	RequestedAt    time.Time `json:"requested_at" db:"requested_at"`
	RespondedAt    time.Time `json:"responded_at" db:"responded_at"`
	ResponseTimeMs int64     `json:"response_time_ms" db:"response_time_ms"`

	InputTokens           int `json:"input_tokens" db:"input_tokens"`
	OutputTokens          int `json:"output_tokens" db:"output_tokens"`
	CachedTokens          int `json:"cached_tokens" db:"cached_tokens"`
	CacheCreationTokens   int `json:"cache_creation_tokens" db:"cache_creation_tokens"`
	CacheCreation5mTokens int `json:"cache_creation_5m_tokens" db:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int `json:"cache_creation_1h_tokens" db:"cache_creation_1h_tokens"`

	InputCost         float64 `json:"input_cost" db:"input_cost"`
	CachedCost        float64 `json:"cached_cost" db:"cached_cost"`
	CacheCreationCost float64 `json:"cache_creation_cost" db:"cache_creation_cost"`
	OutputCost        float64 `json:"output_cost" db:"output_cost"`
	TotalCost         float64 `json:"total_cost" db:"total_cost"`

	StatusCode   int     `json:"status_code" db:"status_code"`
	ErrorMessage *string `json:"error_message,omitempty" db:"error_message"`

	RawMetadata     map[string]string `json:"raw_metadata,omitempty" db:"raw_metadata"`
	IndexedMetadata map[string]string `json:"indexed_metadata,omitempty" db:"indexed_metadata"`
	// BodyS3Key points to the archived request/response bodies in object storage
	// (S3/GCS), when body archival is enabled; nil otherwise.
	BodyS3Key       *string `json:"body_s3_key,omitempty" db:"body_s3_key"`
	ModelAliasFound bool    `json:"model_alias_found" db:"model_alias_found"`

	CreatedAt time.Time `json:"created_at" db:"created_at"`

	// Deprecated model redirect — true when the gateway substituted a replacement
	// model; OriginalModel records what the caller originally requested.
	DeprecatedModelRedirected bool    `json:"deprecated_model_redirected" db:"deprecated_model_redirected"`
	OriginalModel             *string `json:"original_model,omitempty" db:"original_model"`

	// Provider-routing decision trace — populated only when the request was routed
	// (x-majordomo-provider: majordomo). RoutedProvider is the chosen upstream,
	// RoutingReason the human-readable rationale, and RoutingOriginalModel the
	// canonical slug the client requested before the model was rewritten.
	RoutedProvider       *string `json:"routed_provider,omitempty" db:"routed_provider"`
	RoutingReason        *string `json:"routing_reason,omitempty" db:"routing_reason"`
	RoutingOriginalModel *string `json:"routing_original_model,omitempty" db:"routing_original_model"`

	// Agent-run observability (Tier 1) — populated only when the request carries an
	// X-Majordomo-Trace-Id header. span_path is the canonical "/"-joined ancestor step
	// names; span_id is the leaf's own id; parent_span_id is the deterministic id of the
	// interior step named by span_path (see internal/spanid).
	TraceID      *string    `json:"trace_id,omitempty"       db:"trace_id"`
	SpanPath     *string    `json:"span_path,omitempty"      db:"span_path"`
	SpanName     *string    `json:"span_name,omitempty"      db:"span_name"`
	SpanID       *uuid.UUID `json:"span_id,omitempty"        db:"span_id"`
	ParentSpanID *uuid.UUID `json:"parent_span_id,omitempty" db:"parent_span_id"`
}
