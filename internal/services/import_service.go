package services

import (
	"context"
	"math"
	"strings"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
)

const (
	// maxImportRequests and maxImportToolSpans bound one import call.
	maxImportRequests  = 20000
	maxImportToolSpans = 20000

	// claudeCodeTracePrefix namespaces imported sessions' trace ids.
	claudeCodeTracePrefix = "claude-code:"
	// claudeCodeRequestPath / claudeCodeRequestMethod mark imported rows in the
	// request log, which otherwise only holds proxied HTTP requests.
	claudeCodeRequestPath   = "claude-code/session"
	claudeCodeRequestMethod = "IMPORT"
	claudeCodeProvider      = "anthropic"
)

// importNamespace derives deterministic ids for imported rows, so importing the
// same session again finds the rows it already stored. It must never change.
var importNamespace = uuid.MustParse("3c9d4e5f-0000-5000-a000-00000000cc01")

// CostCalculator prices token usage. Satisfied by *pricing.Service.
type CostCalculator interface {
	Calculate(metrics *models.UsageMetrics) models.Cost
}

// ImportService imports usage recorded outside the proxy, e.g. a Claude Code
// session run on a subscription, as API-equivalent request-log rows and tool
// spans, so it shows up in usage reports and run waterfalls. Costs are always
// computed here from token counts; clients never report a cost.
type ImportService struct {
	repo    repositories.ImportStorage
	pricing CostCalculator
}

// NewImportService constructs an ImportService.
func NewImportService(repo repositories.ImportStorage, pricing CostCalculator) *ImportService {
	return &ImportService{repo: repo, pricing: pricing}
}

// ImportClaudeCode stores one Claude Code session's requests and tool spans,
// attributed to apiKeyID. The session becomes a key-scoped run.
// Re-importing a session is safe: requests already stored are skipped and tool
// spans are updated (their re-read counts grow as the session continues).
func (s *ImportService) ImportClaudeCode(ctx context.Context, apiKeyID uuid.UUID, imp *models.ClaudeCodeImport) (*models.ClaudeCodeImportResult, error) {
	if err := validateClaudeCodeImport(imp); err != nil {
		return nil, err
	}
	traceID := claudeCodeTracePrefix + apiKeyID.String() + ":" + imp.SessionID
	// Row ids are derived from the importing key as well as the session, so the
	// same session imported under another key can never touch these rows.
	identity := apiKeyID.String() + "\x00" + imp.SessionID
	metadata := map[string]string{}
	for k, v := range imp.Metadata {
		metadata[k] = v
	}

	metadata["source"] = "claude-code"
	result := &models.ClaudeCodeImportResult{
		TraceID:   traceID,
		Requests:  len(imp.Requests),
		ToolSpans: len(imp.ToolSpans),
	}

	logs := make([]*models.RequestLog, 0, len(imp.Requests))
	for i := range imp.Requests {
		log := s.requestLog(apiKeyID, traceID, identity, metadata, &imp.Requests[i])
		if !log.ModelAliasFound {
			result.Unpriced++
		}
		result.TotalCost += log.TotalCost
		logs = append(logs, log)
	}

	spans := make([]*models.ToolSpan, 0, len(imp.ToolSpans))
	for i := range imp.ToolSpans {
		span := s.toolSpan(apiKeyID, traceID, identity, &imp.ToolSpans[i])
		result.ToolCost += span.AttributedCost
		spans = append(spans, span)
	}

	var err error
	result.RequestsInserted, result.ToolSpansWritten, err = s.repo.SaveImport(ctx, logs, spans)
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *ImportService) requestLog(apiKeyID uuid.UUID, traceID, identity string, metadata map[string]string, r *models.ClaudeCodeImportRequest) *models.RequestLog {
	cacheCreation := r.CacheCreation5mTokens + r.CacheCreation1hTokens
	metrics := &models.UsageMetrics{
		Provider:              claudeCodeProvider,
		Model:                 r.Model,
		InputTokens:           r.InputTokens + r.CacheReadTokens + cacheCreation,
		OutputTokens:          r.OutputTokens,
		CachedTokens:          r.CacheReadTokens,
		CacheCreationTokens:   cacheCreation,
		CacheCreation5mTokens: r.CacheCreation5mTokens,
		CacheCreation1hTokens: r.CacheCreation1hTokens,
	}
	cost := s.pricing.Calculate(metrics)

	id := uuid.NewSHA1(importNamespace, []byte("claude-code\x00"+identity+"\x00"+r.RequestID))
	path := spanid.CanonicalPath(r.SpanPath)
	parent := spanid.InteriorSpanID(traceID, path)
	spanName := r.Model

	return &models.RequestLog{
		ID:                    id,
		MajordomoAPIKeyID:     &apiKeyID,
		Provider:              claudeCodeProvider,
		Model:                 r.Model,
		RequestPath:           claudeCodeRequestPath,
		RequestMethod:         claudeCodeRequestMethod,
		RequestedAt:           r.OccurredAt,
		RespondedAt:           r.OccurredAt,
		InputTokens:           metrics.InputTokens,
		OutputTokens:          r.OutputTokens,
		CachedTokens:          r.CacheReadTokens,
		CacheCreationTokens:   cacheCreation,
		CacheCreation5mTokens: r.CacheCreation5mTokens,
		CacheCreation1hTokens: r.CacheCreation1hTokens,
		InputCost:             cost.InputCost,
		CachedCost:            cost.CachedCost,
		CacheCreationCost:     cost.CacheCreationCost,
		OutputCost:            cost.OutputCost,
		TotalCost:             cost.TotalCost,
		StatusCode:            200,
		RawMetadata:           metadata,
		ModelAliasFound:       cost.ModelAliasFound,
		TraceID:               &traceID,
		SpanPath:              &path,
		SpanName:              &spanName,
		SpanID:                &id,
		ParentSpanID:          &parent,
	}
}

// toolSpan prices each carrying request independently.
func (s *ImportService) toolSpan(apiKeyID uuid.UUID, traceID, identity string, t *models.ClaudeCodeImportToolSpan) *models.ToolSpan {
	var cost float64
	var rereads int
	for i, c := range t.Charges {
		cost += s.pricing.Calculate(c.Metrics(claudeCodeProvider)).TotalCost
		if i > 0 && c.Input > 0 {
			rereads++
		}
	}
	path := spanid.CanonicalPath(t.SpanPath)
	return &models.ToolSpan{
		ID:                uuid.NewSHA1(importNamespace, []byte("claude-code-tool\x00"+identity+"\x00"+t.ToolUseID)),
		MajordomoAPIKeyID: &apiKeyID,
		TraceID:           traceID,
		SpanPath:          path,
		ParentSpanID:      spanid.InteriorSpanID(traceID, path),
		ToolName:          t.ToolName,
		MCPServer:         mcpServer(t.ToolName),
		Provider:          claudeCodeProvider,
		Model:             t.Model,
		CalledAt:          t.CalledAt,
		ResultTokens:      t.ResultTokens,

		RereadCount:    rereads,
		AttributedCost: cost,
	}
}

// mcpServer returns the MCP server of a tool named "mcp__<server>__<tool>".
func mcpServer(tool string) *string {
	parts := strings.SplitN(tool, "__", 3)
	if len(parts) != 3 || parts[0] != "mcp" || parts[1] == "" {
		return nil
	}
	return &parts[1]
}

func validateClaudeCodeImport(imp *models.ClaudeCodeImport) error {
	switch {
	case imp.SessionID == "" || len(imp.SessionID) > 128:
		return validationErrorf("session_id is required (max 128 chars)")
	case len(imp.Requests) == 0:
		return validationErrorf("requests is empty")
	case len(imp.Requests) > maxImportRequests:
		return validationErrorf("too many requests (max %d)", maxImportRequests)
	case len(imp.ToolSpans) > maxImportToolSpans:
		return validationErrorf("too many tool_spans (max %d)", maxImportToolSpans)
	case len(imp.Metadata) > 10:
		return validationErrorf("too many metadata keys (max 10)")
	}
	for k, v := range imp.Metadata {
		if k == "" || len(k) > 64 || len(v) > 255 {
			return validationErrorf("metadata keys must be 1-64 chars and values at most 255 chars")
		}
	}
	for i, r := range imp.Requests {
		switch {
		case r.RequestID == "" || len(r.RequestID) > 255:
			return validationErrorf("requests[%d]: request_id is required (max 255 chars)", i)
		case r.Model == "" || len(r.Model) > 100:
			return validationErrorf("requests[%d]: model is required (max 100 chars)", i)
		case r.OccurredAt.IsZero():
			return validationErrorf("requests[%d]: occurred_at is required", i)
		case len(r.SpanPath) > 1024:
			return validationErrorf("requests[%d]: span_path too long", i)
		case r.InputTokens > math.MaxInt32 || r.CacheReadTokens > math.MaxInt32 || r.CacheCreation5mTokens > math.MaxInt32 || r.CacheCreation1hTokens > math.MaxInt32 || r.OutputTokens > math.MaxInt32:
			return validationErrorf("requests[%d]: token count exceeds storage limit", i)
		case r.InputTokens < 0 || r.OutputTokens < 0 || r.CacheReadTokens < 0 ||
			r.CacheCreation5mTokens < 0 || r.CacheCreation1hTokens < 0:
			return validationErrorf("requests[%d]: token counts must be non-negative", i)
		case int64(r.InputTokens)+int64(r.CacheReadTokens)+int64(r.CacheCreation5mTokens)+int64(r.CacheCreation1hTokens) > math.MaxInt32:
			return validationErrorf("requests[%d]: prompt exceeds storage limit", i)
		}
	}
	for i, t := range imp.ToolSpans {
		if len(t.Charges) == 0 || len(t.Charges) > maxImportRequests || t.Charges[0].Model != t.Model {
			return validationErrorf("tool_spans[%d]: expected per-request charges starting with the carrying model", i)
		}
		for _, c := range t.Charges {
			if !c.Valid() || c.Input > t.ResultTokens {
				return validationErrorf("tool_spans[%d]: invalid charge", i)
			}
		}
		switch {
		case t.ToolUseID == "" || len(t.ToolUseID) > 255:
			return validationErrorf("tool_spans[%d]: tool_use_id is required (max 255 chars)", i)
		case t.ToolName == "" || len(t.ToolName) > 255:
			return validationErrorf("tool_spans[%d]: tool_name is required (max 255 chars)", i)
		case t.Model == "" || len(t.Model) > 100:
			return validationErrorf("tool_spans[%d]: model is required (max 100 chars)", i)
		case t.CalledAt.IsZero():
			return validationErrorf("tool_spans[%d]: called_at is required", i)
		case len(t.SpanPath) > 1024:
			return validationErrorf("tool_spans[%d]: span_path too long", i)
		case t.ResultTokens < 0 || t.ResultTokens > math.MaxInt32:
			return validationErrorf("tool_spans[%d]: invalid token counts", i)
		}
	}
	return nil
}
