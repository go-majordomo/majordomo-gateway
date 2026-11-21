package repositories

import (
	"context"
	"fmt"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// GetMCPServerUsage aggregates spans before matching session metadata. A
// session can have many request rows and the same trace ID can occur under
// different keys. Each span must contribute exactly once.
func (r *UsageRepository) GetMCPServerUsage(ctx context.Context, filter *UsageFilter) ([]models.MCPServerUsage, error) {
	rows := []models.MCPServerUsage{}
	if filter.ExcludeRuns || filter.StatusClass == "error" {
		return rows, nil
	} // imported requests have status 200
	query := `
 WITH spans AS (
  SELECT trace_id, majordomo_api_key_id, mcp_server, count(*) AS calls,
         sum(result_tokens) AS tokens, sum(reread_count) AS rereads, sum(attributed_cost) AS cost
  FROM run_tool_spans
  WHERE mcp_server IS NOT NULL AND called_at >= $1 AND called_at < $2
    AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
    AND ($4::text IS NULL OR provider = $4)
    AND ($5::text IS NULL OR model = $5)
  GROUP BY trace_id, majordomo_api_key_id, mcp_server
 ), sessions AS (
  SELECT r.trace_id, r.majordomo_api_key_id, bool_or(r.raw_metadata->>'usage_basis'='estimated') AS estimated
  FROM (SELECT DISTINCT trace_id, majordomo_api_key_id FROM spans) k
  JOIN llm_requests r ON r.trace_id=k.trace_id AND r.majordomo_api_key_id IS NOT DISTINCT FROM k.majordomo_api_key_id
  WHERE r.request_method='IMPORT'`
	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query += `
  GROUP BY r.trace_id, r.majordomo_api_key_id
 )
 SELECT s.mcp_server, sum(s.calls) AS calls, sum(s.tokens) AS result_tokens,
        sum(s.rereads) AS rereads, sum(s.cost) AS attributed_cost,
        coalesce(sum(s.calls) FILTER (WHERE sessions.estimated),0) AS estimated_usage_calls
 FROM spans s JOIN sessions ON sessions.trace_id=s.trace_id
  AND sessions.majordomo_api_key_id IS NOT DISTINCT FROM s.majordomo_api_key_id
 GROUP BY s.mcp_server ORDER BY attributed_cost DESC, s.mcp_server`
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get MCP server usage: %w", err)
	}
	return rows, nil
}
