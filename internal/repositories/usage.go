package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
)

// runTreeFetchCap bounds the number of leaves loaded when assembling a single run's
// tree. A run larger than this is rendered truncated rather than risking an unbounded
// scan/allocation.
const runTreeFetchCap = 5000

// maxSpanDepth bounds the interior-node depth materialized per leaf, guarding against
// a pathologically deep span_path. Deeper paths are attached at this depth.
const maxSpanDepth = 64

// MetadataFilter represents a single key=value filter on indexed_metadata.
type MetadataFilter struct {
	Key   string
	Value string
}

// UsageFilter holds common filters for usage reporting queries. The gateway is
// single-tenant, so there is no org/user ownership scoping — filtering is by time
// range plus optional API key, provider, model, status class, and metadata.
type UsageFilter struct {
	Start           time.Time
	End             time.Time
	APIKeyID        *uuid.UUID
	Provider        *string
	Model           *string
	StatusClass     string           // "" (all), "error" (status >= 400), or "success" (status < 400)
	MetadataFilters []MetadataFilter // AND of up to 2 key=value pairs
	ExcludeRuns     bool             // when true, omit requests that belong to an agent run (trace_id set)
}

// appendStatusClass appends a status-code class predicate for the log list.
func appendStatusClass(query, statusClass string) string {
	switch statusClass {
	case "error":
		return query + ` AND status_code >= 400`
	case "success":
		return query + ` AND status_code < 400`
	default:
		return query
	}
}

// appendExcludeRuns appends a predicate that omits requests belonging to a run.
func appendExcludeRuns(query string, exclude bool) string {
	if exclude {
		return query + ` AND trace_id IS NULL`
	}
	return query
}

// appendMetadataFilters appends AND indexed_metadata->>$N = $M clauses for each metadata filter.
func appendMetadataFilters(query string, args []interface{}, filters []MetadataFilter) (string, []interface{}) {
	for _, f := range filters {
		query += fmt.Sprintf(` AND indexed_metadata->>$%d = $%d`, len(args)+1, len(args)+2)
		args = append(args, f.Key, f.Value)
	}
	return query, args
}

// UsageStorage is the interface satisfied by UsageRepository.
type UsageStorage interface {
	GetUsageSummary(ctx context.Context, filter *UsageFilter) (*models.UsageSummary, error)
	GetDailyUsage(ctx context.Context, filter *UsageFilter) ([]*models.DailyUsage, error)
	GetModelBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.ModelUsage, error)
	GetProviderBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.ProviderUsage, error)
	GetAPIKeyBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.APIKeyUsage, error)
	GetErrorSummary(ctx context.Context, filter *UsageFilter) (*models.ErrorSummary, error)
	GetRecentErrors(ctx context.Context, filter *UsageFilter, limit int) ([]*models.RecentError, error)
	GetLatencyStats(ctx context.Context, filter *UsageFilter) (*models.LatencyStats, error)
	ListUsageRequests(ctx context.Context, filter *UsageFilter, limit, offset int) ([]*models.RequestListItem, int, error)
	GetMetadataBreakdown(ctx context.Context, filter *UsageFilter, keyName string) ([]*models.MetadataBreakdown, error)
	GetRequestDetail(ctx context.Context, requestID uuid.UUID) (*models.RequestLog, error)
	ListRuns(ctx context.Context, filter *UsageFilter, limit, offset int) ([]*models.RunListItem, int, error)
	GetRunTree(ctx context.Context, filter *UsageFilter, traceID string) (*models.RunTree, error)
}

// UsageRepository handles usage reporting data access.
type UsageRepository struct {
	db *sqlx.DB
}

// NewUsageRepository constructs a UsageRepository backed by the given database.
func NewUsageRepository(db *sqlx.DB) *UsageRepository {
	return &UsageRepository{db: db}
}

const requestListItemColumns = `id, majordomo_api_key_id, provider, model, requested_at, response_time_ms, input_tokens, output_tokens, cached_tokens, cache_creation_tokens, total_cost, status_code, error_message, trace_id`

// GetUsageSummary returns aggregated usage totals for the given filter.
func (r *UsageRepository) GetUsageSummary(ctx context.Context, filter *UsageFilter) (*models.UsageSummary, error) {
	query := `
		SELECT
			COUNT(*) AS total_requests,
			COALESCE(SUM(input_tokens), 0) AS total_input_tokens,
			COALESCE(SUM(output_tokens), 0) AS total_output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS total_cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS total_cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS total_cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS total_cache_creation_cost
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)

	var summary struct {
		TotalRequests            int64   `db:"total_requests"`
		TotalInputTokens         int64   `db:"total_input_tokens"`
		TotalOutputTokens        int64   `db:"total_output_tokens"`
		TotalCachedTokens        int64   `db:"total_cached_tokens"`
		TotalCacheCreationTokens int64   `db:"total_cache_creation_tokens"`
		TotalCost                float64 `db:"total_cost"`
		TotalCachedCost          float64 `db:"total_cached_cost"`
		TotalCacheCreationCost   float64 `db:"total_cache_creation_cost"`
	}
	if err := r.db.GetContext(ctx, &summary, query, args...); err != nil {
		return nil, fmt.Errorf("get usage summary: %w", err)
	}

	return &models.UsageSummary{
		TotalRequests:            summary.TotalRequests,
		TotalInputTokens:         summary.TotalInputTokens,
		TotalOutputTokens:        summary.TotalOutputTokens,
		TotalCachedTokens:        summary.TotalCachedTokens,
		TotalCacheCreationTokens: summary.TotalCacheCreationTokens,
		TotalCost:                summary.TotalCost,
		TotalCachedCost:          summary.TotalCachedCost,
		TotalCacheCreationCost:   summary.TotalCacheCreationCost,
	}, nil
}

// GetDailyUsage returns per-day usage aggregates for the given filter.
func (r *UsageRepository) GetDailyUsage(ctx context.Context, filter *UsageFilter) ([]*models.DailyUsage, error) {
	query := `
		SELECT
			TO_CHAR(DATE_TRUNC('day', requested_at), 'YYYY-MM-DD') AS date,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS cache_creation_cost
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query += `
		GROUP BY DATE_TRUNC('day', requested_at)
		ORDER BY date`

	type row struct {
		Date                string  `db:"date"`
		RequestCount        int64   `db:"request_count"`
		InputTokens         int64   `db:"input_tokens"`
		OutputTokens        int64   `db:"output_tokens"`
		CachedTokens        int64   `db:"cached_tokens"`
		CacheCreationTokens int64   `db:"cache_creation_tokens"`
		TotalCost           float64 `db:"total_cost"`
		CachedCost          float64 `db:"cached_cost"`
		CacheCreationCost   float64 `db:"cache_creation_cost"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get daily usage: %w", err)
	}

	result := make([]*models.DailyUsage, len(rows))
	for i, row := range rows {
		result[i] = &models.DailyUsage{
			Date:                row.Date,
			RequestCount:        row.RequestCount,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalCost:           row.TotalCost,
			CachedCost:          row.CachedCost,
			CacheCreationCost:   row.CacheCreationCost,
		}
	}
	return result, nil
}

// GetModelBreakdown returns usage aggregates grouped by provider and model.
func (r *UsageRepository) GetModelBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.ModelUsage, error) {
	query := `
		SELECT
			provider,
			model,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS cache_creation_cost
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query += `
		GROUP BY provider, model
		ORDER BY total_cost DESC`

	type row struct {
		Provider            string  `db:"provider"`
		Model               string  `db:"model"`
		RequestCount        int64   `db:"request_count"`
		InputTokens         int64   `db:"input_tokens"`
		OutputTokens        int64   `db:"output_tokens"`
		CachedTokens        int64   `db:"cached_tokens"`
		CacheCreationTokens int64   `db:"cache_creation_tokens"`
		TotalCost           float64 `db:"total_cost"`
		CachedCost          float64 `db:"cached_cost"`
		CacheCreationCost   float64 `db:"cache_creation_cost"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get model breakdown: %w", err)
	}

	result := make([]*models.ModelUsage, len(rows))
	for i, row := range rows {
		result[i] = &models.ModelUsage{
			Provider:            row.Provider,
			Model:               row.Model,
			RequestCount:        row.RequestCount,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalCost:           row.TotalCost,
			CachedCost:          row.CachedCost,
			CacheCreationCost:   row.CacheCreationCost,
		}
	}
	return result, nil
}

// GetProviderBreakdown returns usage aggregates grouped by provider only.
func (r *UsageRepository) GetProviderBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.ProviderUsage, error) {
	query := `
		SELECT
			provider,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS cache_creation_cost
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query += `
		GROUP BY provider
		ORDER BY total_cost DESC`

	type row struct {
		Provider            string  `db:"provider"`
		RequestCount        int64   `db:"request_count"`
		InputTokens         int64   `db:"input_tokens"`
		OutputTokens        int64   `db:"output_tokens"`
		CachedTokens        int64   `db:"cached_tokens"`
		CacheCreationTokens int64   `db:"cache_creation_tokens"`
		TotalCost           float64 `db:"total_cost"`
		CachedCost          float64 `db:"cached_cost"`
		CacheCreationCost   float64 `db:"cache_creation_cost"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get provider breakdown: %w", err)
	}

	result := make([]*models.ProviderUsage, len(rows))
	for i, row := range rows {
		result[i] = &models.ProviderUsage{
			Provider:            row.Provider,
			RequestCount:        row.RequestCount,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalCost:           row.TotalCost,
			CachedCost:          row.CachedCost,
			CacheCreationCost:   row.CacheCreationCost,
		}
	}
	return result, nil
}

// GetAPIKeyBreakdown returns usage aggregates grouped by API key.
func (r *UsageRepository) GetAPIKeyBreakdown(ctx context.Context, filter *UsageFilter) ([]*models.APIKeyUsage, error) {
	query := `
		SELECT
			lr.majordomo_api_key_id AS api_key_id,
			ak.name AS api_key_name,
			COUNT(*) AS request_count,
			COALESCE(SUM(lr.input_tokens), 0) AS input_tokens,
			COALESCE(SUM(lr.output_tokens), 0) AS output_tokens,
			COALESCE(SUM(lr.cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(lr.cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(lr.total_cost), 0) AS total_cost,
			COALESCE(SUM(lr.cached_cost), 0) AS cached_cost,
			COALESCE(SUM(lr.cache_creation_cost), 0) AS cache_creation_cost
		FROM llm_requests lr
		JOIN api_keys ak ON ak.id = lr.majordomo_api_key_id
		WHERE lr.requested_at >= $1 AND lr.requested_at < $2
			AND ($3::text IS NULL OR lr.provider = $3)
			AND ($4::text IS NULL OR lr.model = $4)`

	args := []interface{}{filter.Start, filter.End, filter.Provider, filter.Model}
	// Metadata filters use the non-aliased column since it is unambiguous on llm_requests.
	for _, f := range filter.MetadataFilters {
		query += fmt.Sprintf(` AND lr.indexed_metadata->>$%d = $%d`, len(args)+1, len(args)+2)
		args = append(args, f.Key, f.Value)
	}
	query += `
		GROUP BY lr.majordomo_api_key_id, ak.name
		ORDER BY total_cost DESC`

	type row struct {
		APIKeyID            uuid.UUID `db:"api_key_id"`
		APIKeyName          string    `db:"api_key_name"`
		RequestCount        int64     `db:"request_count"`
		InputTokens         int64     `db:"input_tokens"`
		OutputTokens        int64     `db:"output_tokens"`
		CachedTokens        int64     `db:"cached_tokens"`
		CacheCreationTokens int64     `db:"cache_creation_tokens"`
		TotalCost           float64   `db:"total_cost"`
		CachedCost          float64   `db:"cached_cost"`
		CacheCreationCost   float64   `db:"cache_creation_cost"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get api key breakdown: %w", err)
	}

	result := make([]*models.APIKeyUsage, len(rows))
	for i, row := range rows {
		result[i] = &models.APIKeyUsage{
			APIKeyID:            row.APIKeyID,
			APIKeyName:          row.APIKeyName,
			RequestCount:        row.RequestCount,
			InputTokens:         row.InputTokens,
			OutputTokens:        row.OutputTokens,
			CachedTokens:        row.CachedTokens,
			CacheCreationTokens: row.CacheCreationTokens,
			TotalCost:           row.TotalCost,
			CachedCost:          row.CachedCost,
			CacheCreationCost:   row.CacheCreationCost,
		}
	}
	return result, nil
}

// GetErrorSummary returns error-rate totals plus a per-day error-rate series for the filter.
func (r *UsageRepository) GetErrorSummary(ctx context.Context, filter *UsageFilter) (*models.ErrorSummary, error) {
	// Overall totals.
	summaryQuery := `
		SELECT
			COUNT(*) AS total_requests,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_requests
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	summaryArgs := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	summaryQuery, summaryArgs = appendMetadataFilters(summaryQuery, summaryArgs, filter.MetadataFilters)

	var totals struct {
		TotalRequests int64 `db:"total_requests"`
		ErrorRequests int64 `db:"error_requests"`
	}
	if err := r.db.GetContext(ctx, &totals, summaryQuery, summaryArgs...); err != nil {
		return nil, fmt.Errorf("get error summary: %w", err)
	}

	// Per-day series.
	dailyQuery := `
		SELECT
			TO_CHAR(DATE_TRUNC('day', requested_at), 'YYYY-MM-DD') AS date,
			COUNT(*) AS request_count,
			COALESCE(SUM(CASE WHEN status_code >= 400 THEN 1 ELSE 0 END), 0) AS error_count
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	dailyArgs := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	dailyQuery, dailyArgs = appendMetadataFilters(dailyQuery, dailyArgs, filter.MetadataFilters)
	dailyQuery += `
		GROUP BY DATE_TRUNC('day', requested_at)
		ORDER BY date`

	type dailyRow struct {
		Date         string `db:"date"`
		RequestCount int64  `db:"request_count"`
		ErrorCount   int64  `db:"error_count"`
	}
	var dailyRows []dailyRow
	if err := r.db.SelectContext(ctx, &dailyRows, dailyQuery, dailyArgs...); err != nil {
		return nil, fmt.Errorf("get daily error rate: %w", err)
	}

	daily := make([]*models.DailyErrorRate, len(dailyRows))
	for i, row := range dailyRows {
		daily[i] = &models.DailyErrorRate{
			Date:         row.Date,
			RequestCount: row.RequestCount,
			ErrorCount:   row.ErrorCount,
			ErrorRate:    errorRate(row.ErrorCount, row.RequestCount),
		}
	}

	return &models.ErrorSummary{
		TotalRequests: totals.TotalRequests,
		ErrorRequests: totals.ErrorRequests,
		ErrorRate:     errorRate(totals.ErrorRequests, totals.TotalRequests),
		Daily:         daily,
	}, nil
}

// errorRate returns errors/total as a fraction, guarding divide-by-zero.
func errorRate(errCount, total int64) float64 {
	if total == 0 {
		return 0
	}
	return float64(errCount) / float64(total)
}

// GetRecentErrors returns the most recent failed requests (status >= 400), tagged by API key name.
func (r *UsageRepository) GetRecentErrors(ctx context.Context, filter *UsageFilter, limit int) ([]*models.RecentError, error) {
	query := `
		SELECT
			lr.id AS id,
			lr.majordomo_api_key_id AS api_key_id,
			ak.name AS api_key_name,
			lr.provider AS provider,
			lr.model AS model,
			lr.status_code AS status_code,
			lr.error_message AS error_message,
			lr.requested_at AS requested_at
		FROM llm_requests lr
		LEFT JOIN api_keys ak ON ak.id = lr.majordomo_api_key_id
		WHERE lr.requested_at >= $1 AND lr.requested_at < $2
			AND ($3::uuid IS NULL OR lr.majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR lr.provider = $4)
			AND ($5::text IS NULL OR lr.model = $5)
			AND lr.status_code >= 400`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	for _, f := range filter.MetadataFilters {
		query += fmt.Sprintf(` AND lr.indexed_metadata->>$%d = $%d`, len(args)+1, len(args)+2)
		args = append(args, f.Key, f.Value)
	}
	query += fmt.Sprintf(`
		ORDER BY lr.requested_at DESC
		LIMIT $%d`, len(args)+1)
	args = append(args, limit)

	var items []*models.RecentError
	if err := r.db.SelectContext(ctx, &items, query, args...); err != nil {
		return nil, fmt.Errorf("get recent errors: %w", err)
	}
	return items, nil
}

// GetLatencyStats returns response-time percentiles plus a per-day percentile series for the filter.
func (r *UsageRepository) GetLatencyStats(ctx context.Context, filter *UsageFilter) (*models.LatencyStats, error) {
	overallQuery := `
		SELECT
			COALESCE(AVG(response_time_ms), 0) AS avg_latency_ms,
			COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY response_time_ms), 0) AS p50_latency_ms,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY response_time_ms), 0) AS p95_latency_ms,
			COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY response_time_ms), 0) AS p99_latency_ms
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	overallArgs := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	overallQuery, overallArgs = appendMetadataFilters(overallQuery, overallArgs, filter.MetadataFilters)

	var overall struct {
		AvgLatencyMs float64 `db:"avg_latency_ms"`
		P50LatencyMs float64 `db:"p50_latency_ms"`
		P95LatencyMs float64 `db:"p95_latency_ms"`
		P99LatencyMs float64 `db:"p99_latency_ms"`
	}
	if err := r.db.GetContext(ctx, &overall, overallQuery, overallArgs...); err != nil {
		return nil, fmt.Errorf("get latency stats: %w", err)
	}

	dailyQuery := `
		SELECT
			TO_CHAR(DATE_TRUNC('day', requested_at), 'YYYY-MM-DD') AS date,
			COALESCE(AVG(response_time_ms), 0) AS avg_latency_ms,
			COALESCE(PERCENTILE_CONT(0.5)  WITHIN GROUP (ORDER BY response_time_ms), 0) AS p50_latency_ms,
			COALESCE(PERCENTILE_CONT(0.95) WITHIN GROUP (ORDER BY response_time_ms), 0) AS p95_latency_ms,
			COALESCE(PERCENTILE_CONT(0.99) WITHIN GROUP (ORDER BY response_time_ms), 0) AS p99_latency_ms
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	dailyArgs := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	dailyQuery, dailyArgs = appendMetadataFilters(dailyQuery, dailyArgs, filter.MetadataFilters)
	dailyQuery += `
		GROUP BY DATE_TRUNC('day', requested_at)
		ORDER BY date`

	type dailyRow struct {
		Date         string  `db:"date"`
		AvgLatencyMs float64 `db:"avg_latency_ms"`
		P50LatencyMs float64 `db:"p50_latency_ms"`
		P95LatencyMs float64 `db:"p95_latency_ms"`
		P99LatencyMs float64 `db:"p99_latency_ms"`
	}
	var dailyRows []dailyRow
	if err := r.db.SelectContext(ctx, &dailyRows, dailyQuery, dailyArgs...); err != nil {
		return nil, fmt.Errorf("get daily latency stats: %w", err)
	}

	daily := make([]*models.DailyLatency, len(dailyRows))
	for i, row := range dailyRows {
		daily[i] = &models.DailyLatency{
			Date:         row.Date,
			AvgLatencyMs: row.AvgLatencyMs,
			P50LatencyMs: row.P50LatencyMs,
			P95LatencyMs: row.P95LatencyMs,
			P99LatencyMs: row.P99LatencyMs,
		}
	}

	return &models.LatencyStats{
		AvgLatencyMs: overall.AvgLatencyMs,
		P50LatencyMs: overall.P50LatencyMs,
		P95LatencyMs: overall.P95LatencyMs,
		P99LatencyMs: overall.P99LatencyMs,
		Daily:        daily,
	}, nil
}

// ListUsageRequests returns a paginated list of request records matching the filter.
func (r *UsageRepository) ListUsageRequests(ctx context.Context, filter *UsageFilter, limit, offset int) ([]*models.RequestListItem, int, error) {
	countQuery := `
		SELECT COUNT(*)
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	countArgs := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	countQuery, countArgs = appendMetadataFilters(countQuery, countArgs, filter.MetadataFilters)
	countQuery = appendStatusClass(countQuery, filter.StatusClass)
	countQuery = appendExcludeRuns(countQuery, filter.ExcludeRuns)

	var total int
	if err := r.db.GetContext(ctx, &total, countQuery, countArgs...); err != nil {
		return nil, 0, fmt.Errorf("count usage requests: %w", err)
	}

	query := `
		SELECT ` + requestListItemColumns + `
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
			AND ($4::text IS NULL OR provider = $4)
			AND ($5::text IS NULL OR model = $5)`

	args := []interface{}{filter.Start, filter.End, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query = appendStatusClass(query, filter.StatusClass)
	query = appendExcludeRuns(query, filter.ExcludeRuns)
	query += fmt.Sprintf(`
		ORDER BY requested_at DESC
		LIMIT $%d OFFSET $%d`, len(args)+1, len(args)+2)
	args = append(args, limit, offset)

	var items []*models.RequestListItem
	if err := r.db.SelectContext(ctx, &items, query, args...); err != nil {
		return nil, 0, fmt.Errorf("list usage requests: %w", err)
	}
	return items, total, nil
}

// GetMetadataBreakdown returns usage aggregates grouped by a specific indexed metadata key value.
func (r *UsageRepository) GetMetadataBreakdown(ctx context.Context, filter *UsageFilter, keyName string) ([]*models.MetadataBreakdown, error) {
	query := `
		SELECT
			indexed_metadata->>$3 AS value,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2
			AND ($4::uuid IS NULL OR majordomo_api_key_id = $4)
			AND ($5::text IS NULL OR provider = $5)
			AND ($6::text IS NULL OR model = $6)
			AND indexed_metadata ? $3`

	args := []interface{}{filter.Start, filter.End, keyName, filter.APIKeyID, filter.Provider, filter.Model}
	query, args = appendMetadataFilters(query, args, filter.MetadataFilters)
	query += `
		GROUP BY indexed_metadata->>$3
		ORDER BY total_cost DESC`

	type row struct {
		Value        string  `db:"value"`
		RequestCount int64   `db:"request_count"`
		InputTokens  int64   `db:"input_tokens"`
		OutputTokens int64   `db:"output_tokens"`
		TotalCost    float64 `db:"total_cost"`
	}

	var rows []row
	if err := r.db.SelectContext(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("get metadata breakdown: %w", err)
	}

	result := make([]*models.MetadataBreakdown, len(rows))
	for i, row := range rows {
		result[i] = &models.MetadataBreakdown{
			Value:        row.Value,
			RequestCount: row.RequestCount,
			InputTokens:  row.InputTokens,
			OutputTokens: row.OutputTokens,
			TotalCost:    row.TotalCost,
		}
	}
	return result, nil
}

// GetRequestDetail returns the full request log for a single request, including bodies.
func (r *UsageRepository) GetRequestDetail(ctx context.Context, requestID uuid.UUID) (*models.RequestLog, error) {
	query := `
		SELECT
			id, majordomo_api_key_id, provider_api_key_hash, provider_api_key_alias,
			provider, model, request_path, request_method,
			requested_at, responded_at, response_time_ms,
			input_tokens, output_tokens, cached_tokens, cache_creation_tokens,
			cache_creation_5m_tokens, cache_creation_1h_tokens,
			input_cost, cached_cost, cache_creation_cost, output_cost, total_cost,
			status_code, error_message, raw_metadata, indexed_metadata,
			body_s3_key, model_alias_found,
			deprecated_model_redirected, original_model,
			trace_id, span_path, span_name, span_id, parent_span_id, created_at
		FROM llm_requests WHERE id = $1`

	var (
		log                 models.RequestLog
		rawMetadataJSON     []byte
		indexedMetadataJSON []byte
	)
	err := r.db.QueryRowxContext(ctx, query, requestID).Scan(
		&log.ID, &log.MajordomoAPIKeyID, &log.ProviderAPIKeyHash, &log.ProviderAPIKeyAlias,
		&log.Provider, &log.Model, &log.RequestPath, &log.RequestMethod,
		&log.RequestedAt, &log.RespondedAt, &log.ResponseTimeMs,
		&log.InputTokens, &log.OutputTokens, &log.CachedTokens, &log.CacheCreationTokens,
		&log.CacheCreation5mTokens, &log.CacheCreation1hTokens,
		&log.InputCost, &log.CachedCost, &log.CacheCreationCost, &log.OutputCost, &log.TotalCost,
		&log.StatusCode, &log.ErrorMessage, &rawMetadataJSON, &indexedMetadataJSON,
		&log.BodyS3Key, &log.ModelAliasFound,
		&log.DeprecatedModelRedirected, &log.OriginalModel,
		&log.TraceID, &log.SpanPath, &log.SpanName, &log.SpanID, &log.ParentSpanID, &log.CreatedAt,
	)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrRequestNotFound
		}
		return nil, fmt.Errorf("get request detail: %w", err)
	}

	if rawMetadataJSON != nil {
		log.RawMetadata = make(map[string]string)
		_ = json.Unmarshal(rawMetadataJSON, &log.RawMetadata)
	}
	if indexedMetadataJSON != nil {
		log.IndexedMetadata = make(map[string]string)
		_ = json.Unmarshal(indexedMetadataJSON, &log.IndexedMetadata)
	}

	return &log, nil
}

// ListRuns returns one row per agent run (trace_id) with usage rolled up across every
// LLM call in the run, most recently ended first.
func (r *UsageRepository) ListRuns(ctx context.Context, filter *UsageFilter, limit, offset int) ([]*models.RunListItem, int, error) {
	countQuery := `
		SELECT COUNT(DISTINCT trace_id)
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2 AND trace_id IS NOT NULL
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)`

	var total int
	if err := r.db.GetContext(ctx, &total, countQuery, filter.Start, filter.End, filter.APIKeyID); err != nil {
		return nil, 0, fmt.Errorf("count runs: %w", err)
	}

	// The label is the earliest call's top-level step name, or its span name / model
	// when the run is flat. array_agg(... ORDER BY requested_at) picks that first row
	// without a correlated subquery.
	query := `
		SELECT
			trace_id,
			(array_agg(
				CASE WHEN COALESCE(span_path, '') <> ''
				     THEN split_part(span_path, '/', 1)
				     ELSE COALESCE(span_name, model) END
				ORDER BY requested_at ASC
			))[1] AS label,
			COUNT(*) AS request_count,
			COALESCE(SUM(input_tokens), 0) AS input_tokens,
			COALESCE(SUM(output_tokens), 0) AS output_tokens,
			COALESCE(SUM(cached_tokens), 0) AS cached_tokens,
			COALESCE(SUM(cache_creation_tokens), 0) AS cache_creation_tokens,
			COALESCE(SUM(total_cost), 0) AS total_cost,
			COALESCE(SUM(cached_cost), 0) AS cached_cost,
			COALESCE(SUM(cache_creation_cost), 0) AS cache_creation_cost,
			MIN(requested_at) AS started_at,
			MAX(responded_at) AS ended_at
		FROM llm_requests
		WHERE requested_at >= $1 AND requested_at < $2 AND trace_id IS NOT NULL
			AND ($3::uuid IS NULL OR majordomo_api_key_id = $3)
		GROUP BY trace_id
		ORDER BY ended_at DESC
		LIMIT $4 OFFSET $5`

	var runs []*models.RunListItem
	if err := r.db.SelectContext(ctx, &runs, query, filter.Start, filter.End, filter.APIKeyID, limit, offset); err != nil {
		return nil, 0, fmt.Errorf("list runs: %w", err)
	}
	return runs, total, nil
}

// runLeafRow is the flat projection of one LLM call used to assemble a run tree.
type runLeafRow struct {
	ID             uuid.UUID  `db:"id"`
	SpanID         *uuid.UUID `db:"span_id"`
	ParentSpanID   *uuid.UUID `db:"parent_span_id"`
	SpanPath       string     `db:"span_path"`
	SpanName       string     `db:"span_name"`
	Provider       string     `db:"provider"`
	Model          string     `db:"model"`
	RequestedAt    time.Time  `db:"requested_at"`
	RespondedAt    time.Time  `db:"responded_at"`
	ResponseTimeMs int64      `db:"response_time_ms"`
	StatusCode     int        `db:"status_code"`
	TotalCost      float64    `db:"total_cost"`
}

// GetRunTree fetches every LLM call in a run and assembles the waterfall: a synthetic
// root, one interior node per named tool/agent step (derived from span_path), and the
// LLM calls as leaves, with cost and call count rolled up the tree. Returns (nil, nil)
// when the run has no calls. Interior tool/agent nodes have no rows of their own in
// Tier 1, so they are synthesized here from the leaves' paths.
func (r *UsageRepository) GetRunTree(ctx context.Context, filter *UsageFilter, traceID string) (*models.RunTree, error) {
	query := `
		SELECT
			id, span_id, parent_span_id,
			COALESCE(span_path, '') AS span_path,
			COALESCE(span_name, model) AS span_name,
			provider, model, requested_at, responded_at,
			response_time_ms, status_code, total_cost
		FROM llm_requests
		WHERE trace_id = $1
		ORDER BY requested_at ASC
		LIMIT $2`

	var rows []runLeafRow
	if err := r.db.SelectContext(ctx, &rows, query, traceID, runTreeFetchCap+1); err != nil {
		return nil, fmt.Errorf("fetch run tree: %w", err)
	}
	if len(rows) == 0 {
		return nil, nil
	}

	truncated := false
	if len(rows) > runTreeFetchCap {
		rows = rows[:runTreeFetchCap]
		truncated = true
	}

	return assembleRunTree(traceID, rows, truncated), nil
}

// assembleRunTree builds the waterfall from a run's LLM-call rows, already ordered by
// requested_at ascending. Kept separate from the DB fetch so it can be unit-tested.
func assembleRunTree(traceID string, rows []runLeafRow, truncated bool) *models.RunTree {
	root := &models.RunNode{
		SpanID: spanid.InteriorSpanID(traceID, ""),
		Name:   runLabel(rows[0]),
		Path:   "",
		Kind:   models.RunNodeKindRun,
	}
	nodes := map[string]*models.RunNode{"": root}

	// ensurePath materializes every interior step node on the way to canonicalPath,
	// reusing already-created ancestors, and returns the deepest one.
	ensurePath := func(canonicalPath string) *models.RunNode {
		if n, ok := nodes[canonicalPath]; ok {
			return n
		}
		prefixes := spanid.AncestorPaths(canonicalPath)
		if len(prefixes) > maxSpanDepth {
			prefixes = prefixes[:maxSpanDepth]
		}
		parent := root
		for _, prefix := range prefixes {
			if existing, ok := nodes[prefix]; ok {
				parent = existing
				continue
			}
			segments := spanid.SplitPath(prefix)
			node := &models.RunNode{
				SpanID:       spanid.InteriorSpanID(traceID, prefix),
				ParentSpanID: &parent.SpanID,
				Name:         segments[len(segments)-1],
				Path:         prefix,
				Kind:         models.RunNodeKindStep,
			}
			parent.Children = append(parent.Children, node)
			nodes[prefix] = node
			parent = node
		}
		return parent
	}

	endedAt := rows[0].RespondedAt
	for i := range rows {
		row := &rows[i]
		if row.RespondedAt.After(endedAt) {
			endedAt = row.RespondedAt
		}

		parent := ensurePath(spanid.CanonicalPath(row.SpanPath))

		spanID := row.ID
		if row.SpanID != nil {
			spanID = *row.SpanID
		}
		leaf := &models.RunNode{
			SpanID:         spanID,
			ParentSpanID:   &parent.SpanID,
			Name:           row.SpanName,
			Path:           parent.Path,
			Kind:           models.RunNodeKindLLM,
			RequestID:      &row.ID,
			Provider:       &row.Provider,
			Model:          &row.Model,
			RequestedAt:    &row.RequestedAt,
			RespondedAt:    &row.RespondedAt,
			ResponseTimeMs: &row.ResponseTimeMs,
			StatusCode:     &row.StatusCode,
			SelfCost:       row.TotalCost,
			TotalCost:      row.TotalCost,
			RequestCount:   1,
		}
		parent.Children = append(parent.Children, leaf)
	}

	rollUpRunNode(root)

	return &models.RunTree{
		TraceID:      traceID,
		Root:         root,
		TotalCost:    root.TotalCost,
		RequestCount: root.RequestCount,
		StartedAt:    rows[0].RequestedAt,
		EndedAt:      endedAt,
		Truncated:    truncated,
	}
}

// runLabel derives a run/root label from a leaf: its top-level step name, or its span
// name (already defaulted to the model) when the run is flat.
func runLabel(row runLeafRow) string {
	if segments := spanid.SplitPath(row.SpanPath); len(segments) > 0 {
		return segments[0]
	}
	return row.SpanName
}

// rollUpRunNode sums descendant cost and LLM-call counts into each interior node,
// bottom-up. Leaves already carry their own totals.
func rollUpRunNode(n *models.RunNode) {
	for _, child := range n.Children {
		rollUpRunNode(child)
		n.TotalCost += child.TotalCost
		n.RequestCount += child.RequestCount
	}
}
