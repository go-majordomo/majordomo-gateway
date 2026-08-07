package models

import (
	"time"

	"github.com/google/uuid"
)

// UsageSummary holds aggregate usage metrics for a time range.
type UsageSummary struct {
	TotalRequests            int64   `json:"total_requests"`
	TotalInputTokens         int64   `json:"total_input_tokens"`
	TotalOutputTokens        int64   `json:"total_output_tokens"`
	TotalCachedTokens        int64   `json:"total_cached_tokens"`
	TotalCacheCreationTokens int64   `json:"total_cache_creation_tokens"`
	TotalCost                float64 `json:"total_cost"`
	TotalCachedCost          float64 `json:"total_cached_cost"`
	TotalCacheCreationCost   float64 `json:"total_cache_creation_cost"`
}

// DailyUsage holds per-day usage metrics.
type DailyUsage struct {
	Date                string  `json:"date"`
	RequestCount        int64   `json:"request_count"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalCost           float64 `json:"total_cost"`
	CachedCost          float64 `json:"cached_cost"`
	CacheCreationCost   float64 `json:"cache_creation_cost"`
}

// ModelUsage holds usage metrics grouped by provider and model.
type ModelUsage struct {
	Provider            string  `json:"provider"`
	Model               string  `json:"model"`
	RequestCount        int64   `json:"request_count"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalCost           float64 `json:"total_cost"`
	CachedCost          float64 `json:"cached_cost"`
	CacheCreationCost   float64 `json:"cache_creation_cost"`
}

// APIKeyUsage holds usage metrics grouped by API key.
type APIKeyUsage struct {
	APIKeyID            uuid.UUID `json:"api_key_id"`
	APIKeyName          string    `json:"api_key_name"`
	RequestCount        int64     `json:"request_count"`
	InputTokens         int64     `json:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens"`
	TotalCost           float64   `json:"total_cost"`
	CachedCost          float64   `json:"cached_cost"`
	CacheCreationCost   float64   `json:"cache_creation_cost"`
}

// ProviderUsage holds usage metrics grouped by provider only.
type ProviderUsage struct {
	Provider            string  `json:"provider"`
	RequestCount        int64   `json:"request_count"`
	InputTokens         int64   `json:"input_tokens"`
	OutputTokens        int64   `json:"output_tokens"`
	CachedTokens        int64   `json:"cached_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	TotalCost           float64 `json:"total_cost"`
	CachedCost          float64 `json:"cached_cost"`
	CacheCreationCost   float64 `json:"cache_creation_cost"`
}

// MetadataBreakdown holds usage metrics grouped by a metadata key's values.
type MetadataBreakdown struct {
	Value        string  `json:"value"`
	RequestCount int64   `json:"request_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalCost    float64 `json:"total_cost"`
}

// ErrorSummary holds error-rate aggregates for a time range, with a daily series.
type ErrorSummary struct {
	TotalRequests int64             `json:"total_requests"`
	ErrorRequests int64             `json:"error_requests"`
	ErrorRate     float64           `json:"error_rate"`
	Daily         []*DailyErrorRate `json:"daily"`
}

// DailyErrorRate holds per-day error-rate metrics.
type DailyErrorRate struct {
	Date         string  `json:"date"`
	RequestCount int64   `json:"request_count"`
	ErrorCount   int64   `json:"error_count"`
	ErrorRate    float64 `json:"error_rate"`
}

// RecentError is a lightweight row describing a recent failed request, tagged by API key.
type RecentError struct {
	ID           uuid.UUID  `json:"id" db:"id"`
	APIKeyID     *uuid.UUID `json:"api_key_id,omitempty" db:"api_key_id"`
	APIKeyName   *string    `json:"api_key_name,omitempty" db:"api_key_name"`
	Provider     string     `json:"provider" db:"provider"`
	Model        string     `json:"model" db:"model"`
	StatusCode   int        `json:"status_code" db:"status_code"`
	ErrorMessage *string    `json:"error_message,omitempty" db:"error_message"`
	RequestedAt  time.Time  `json:"requested_at" db:"requested_at"`
}

// LatencyStats holds response-time percentile aggregates for a time range, with a daily series.
type LatencyStats struct {
	AvgLatencyMs float64         `json:"avg_latency_ms"`
	P50LatencyMs float64         `json:"p50_latency_ms"`
	P95LatencyMs float64         `json:"p95_latency_ms"`
	P99LatencyMs float64         `json:"p99_latency_ms"`
	Daily        []*DailyLatency `json:"daily"`
}

// DailyLatency holds per-day response-time percentile metrics.
type DailyLatency struct {
	Date         string  `json:"date"`
	AvgLatencyMs float64 `json:"avg_latency_ms"`
	P50LatencyMs float64 `json:"p50_latency_ms"`
	P95LatencyMs float64 `json:"p95_latency_ms"`
	P99LatencyMs float64 `json:"p99_latency_ms"`
}

// RequestListItem is a lightweight row for the request log table (no bodies).
type RequestListItem struct {
	ID                  uuid.UUID  `json:"id" db:"id"`
	MajordomoAPIKeyID   *uuid.UUID `json:"majordomo_api_key_id,omitempty" db:"majordomo_api_key_id"`
	Provider            string     `json:"provider" db:"provider"`
	Model               string     `json:"model" db:"model"`
	RequestedAt         time.Time  `json:"requested_at" db:"requested_at"`
	ResponseTimeMs      int64      `json:"response_time_ms" db:"response_time_ms"`
	InputTokens         int        `json:"input_tokens" db:"input_tokens"`
	OutputTokens        int        `json:"output_tokens" db:"output_tokens"`
	CachedTokens        int        `json:"cached_tokens" db:"cached_tokens"`
	CacheCreationTokens int        `json:"cache_creation_tokens" db:"cache_creation_tokens"`
	TotalCost           float64    `json:"total_cost" db:"total_cost"`
	StatusCode          int        `json:"status_code" db:"status_code"`
	ErrorMessage        *string    `json:"error_message,omitempty" db:"error_message"`
	// TraceID is set when this request belongs to an agent run; a client can use it
	// to deep-link the row to its run waterfall.
	TraceID *string `json:"trace_id,omitempty" db:"trace_id"`
}

// RunListItem is one agent run/conversation in the runs list, with usage rolled up
// across every LLM call that shares its trace_id.
type RunListItem struct {
	TraceID             string    `json:"trace_id" db:"trace_id"`
	Label               string    `json:"label" db:"label"`
	RequestCount        int64     `json:"request_count" db:"request_count"`
	InputTokens         int64     `json:"input_tokens" db:"input_tokens"`
	OutputTokens        int64     `json:"output_tokens" db:"output_tokens"`
	CachedTokens        int64     `json:"cached_tokens" db:"cached_tokens"`
	CacheCreationTokens int64     `json:"cache_creation_tokens" db:"cache_creation_tokens"`
	TotalCost           float64   `json:"total_cost" db:"total_cost"`
	CachedCost          float64   `json:"cached_cost" db:"cached_cost"`
	CacheCreationCost   float64   `json:"cache_creation_cost" db:"cache_creation_cost"`
	StartedAt           time.Time `json:"started_at" db:"started_at"`
	EndedAt             time.Time `json:"ended_at" db:"ended_at"`
}

// RunNodeKind distinguishes an LLM leaf (a real proxied call) from a synthesized
// interior tool/agent step in the run waterfall.
const (
	RunNodeKindRun  = "run"  // the synthetic root of a run
	RunNodeKindStep = "step" // a tool/agent step (no row of its own in Tier 1)
	RunNodeKindLLM  = "llm"  // a proxied LLM call
)

// RunNode is one node of the run waterfall. Interior ("run"/"step") nodes carry only
// rolled-up totals; "llm" leaves also carry the per-call detail.
type RunNode struct {
	SpanID       uuid.UUID  `json:"span_id"`
	ParentSpanID *uuid.UUID `json:"parent_span_id,omitempty"`
	Name         string     `json:"name"`
	Path         string     `json:"path"` // canonical path-prefix of this node ("" = root)
	Kind         string     `json:"kind"`

	// Leaf-only fields (nil/zero on synthesized interior nodes).
	RequestID      *uuid.UUID `json:"request_id,omitempty"`
	Provider       *string    `json:"provider,omitempty"`
	Model          *string    `json:"model,omitempty"`
	RequestedAt    *time.Time `json:"requested_at,omitempty"`
	RespondedAt    *time.Time `json:"responded_at,omitempty"`
	ResponseTimeMs *int64     `json:"response_time_ms,omitempty"`
	StatusCode     *int       `json:"status_code,omitempty"`

	SelfCost     float64    `json:"self_cost"`     // this node's own cost (0 for interior nodes)
	TotalCost    float64    `json:"total_cost"`    // rolled up over the subtree
	RequestCount int64      `json:"request_count"` // llm leaves in the subtree
	Children     []*RunNode `json:"children"`
}

// RunTree is the assembled waterfall for a single run.
type RunTree struct {
	TraceID      string    `json:"trace_id"`
	Root         *RunNode  `json:"root"`
	TotalCost    float64   `json:"total_cost"`
	RequestCount int64     `json:"request_count"`
	StartedAt    time.Time `json:"started_at"`
	EndedAt      time.Time `json:"ended_at"`
	Truncated    bool      `json:"truncated"` // true if the run exceeded the fetch cap
}
