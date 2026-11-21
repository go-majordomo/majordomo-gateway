package models

import (
	"time"

	"github.com/google/uuid"
)

// ClaudeCodeImport is one Claude Code session's usage, reconstructed from the
// local transcript by `gateway-cli claude-code import` and sent to
// POST /v1/claude-code/import. It carries token counts, model names, tool names
// and span structure only: never prompts, responses, tool inputs or results.
//
// The session becomes one agent run. Span paths name where each call happened,
// e.g. "claude-code/turn-2" for the main agent answering the user's second
// message and "claude-code/turn-2/agent:researcher" for a subagent it launched,
// so the existing run waterfall renders the session. Costs are computed
// server-side from the token counts.
type ClaudeCodeImport struct {
	SessionID string                     `json:"session_id"`
	Metadata  map[string]string          `json:"metadata,omitempty"` // e.g. {"team": "acme"}
	Requests  []ClaudeCodeImportRequest  `json:"requests"`
	ToolSpans []ClaudeCodeImportToolSpan `json:"tool_spans,omitempty"`
}

// ClaudeCodeImportRequest is one model API request made during the session.
// Token fields are disjoint, as Anthropic reports them: InputTokens excludes
// cache reads and cache writes.
type ClaudeCodeImportRequest struct {
	RequestID             string    `json:"request_id"` // Anthropic request id, for dedupe
	SpanPath              string    `json:"span_path"`
	Model                 string    `json:"model"`
	OccurredAt            time.Time `json:"occurred_at"`
	InputTokens           int       `json:"input_tokens"`
	OutputTokens          int       `json:"output_tokens"`
	CacheReadTokens       int       `json:"cache_read_tokens"`
	CacheCreation5mTokens int       `json:"cache_creation_5m_tokens"`
	CacheCreation1hTokens int       `json:"cache_creation_1h_tokens"`
}

// ModelInput is a tool result's share of the input consumed by one model.
type ModelInput struct {
	Model   string `json:"model"`
	Input   int    `json:"input_tokens"`
	Cached  int    `json:"cached_tokens"`
	Write5m int    `json:"write_5m_tokens,omitempty"`
	Write1h int    `json:"write_1h_tokens,omitempty"`
	// Prompt is the size of the request prompt these tokens were part of, which
	// selects the price tier (e.g. long-context rates). Zero selects the base tier.
	Prompt int `json:"prompt_tokens,omitempty"`
}

// ClaudeCodeImportToolSpan records the first carrier and one charge for each request carrying the result.
type ClaudeCodeImportToolSpan struct {
	Charges      []ModelInput `json:"charges"`
	ToolUseID    string       `json:"tool_use_id"`
	SpanPath     string       `json:"span_path"` // the step it ran under
	ToolName     string       `json:"tool_name"` // e.g. "mcp__crm__get_leads"
	Model        string       `json:"model"`
	CalledAt     time.Time    `json:"called_at"`
	ResultTokens int          `json:"result_tokens"`
}

// ClaudeCodeImportResult reports what an import stored.
type ClaudeCodeImportResult struct {
	TraceID          string  `json:"trace_id"`
	Requests         int     `json:"requests"`
	RequestsInserted int     `json:"requests_inserted"`
	ToolSpans        int     `json:"tool_spans"`
	ToolSpansWritten int     `json:"tool_spans_written"`
	Unpriced         int     `json:"unpriced"`
	TotalCost        float64 `json:"total_cost"`
	ToolCost         float64 `json:"tool_cost"`
}

// ToolSpan is a tool call in an agent run (agent-run observability Tier 2). Its
// cost is attributed, not additive: the tokens are already inside the cost of
// the LLM calls that carried them.
type ToolSpan struct {
	ID                uuid.UUID  `db:"id"`
	MajordomoAPIKeyID *uuid.UUID `db:"majordomo_api_key_id"`
	TraceID           string     `db:"trace_id"`
	SpanPath          string     `db:"span_path"`
	ParentSpanID      uuid.UUID  `db:"parent_span_id"`
	ToolName          string     `db:"tool_name"`
	MCPServer         *string    `db:"mcp_server"`
	Provider          string     `db:"provider"`
	Model             string     `db:"model"`
	CalledAt          time.Time  `db:"called_at"`
	ResultTokens      int        `db:"result_tokens"`

	RereadCount    int     `db:"reread_count"`
	AttributedCost float64 `db:"attributed_cost"`
}
