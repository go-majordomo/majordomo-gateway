package repositories

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// InsertRequest writes one complete request row using a database or the caller's transaction.
func InsertRequest(ctx context.Context, db sqlx.ExecerContext, active map[string]bool, log *models.RequestLog, skipExisting bool) (sql.Result, error) {
	// Copy only *active* metadata keys into indexed_metadata; the rest live in
	// raw_metadata only. This keeps the indexed_metadata GIN index bounded.
	indexedMetadata := make(map[string]string)
	for key, value := range log.RawMetadata {
		if active[key] {
			indexedMetadata[key] = value
		}
	}

	rawMetadataJSON, err := json.Marshal(log.RawMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal raw metadata: %w", err)
	}
	indexedMetadataJSON, err := json.Marshal(indexedMetadata)
	if err != nil {
		return nil, fmt.Errorf("marshal indexed metadata: %w", err)
	}

	query := `
		INSERT INTO llm_requests (
			id, majordomo_api_key_id, provider_api_key_hash, provider_api_key_alias,
			provider, model, request_path, request_method,
			requested_at, responded_at, response_time_ms,
			input_tokens, output_tokens, cached_tokens, cache_creation_tokens,
			cache_creation_5m_tokens, cache_creation_1h_tokens,
			input_cost, cached_cost, cache_creation_cost, output_cost, total_cost,
			status_code, error_message, raw_metadata, indexed_metadata,
			body_s3_key, model_alias_found,
			deprecated_model_redirected, original_model,
			trace_id, span_path, span_name, span_id, parent_span_id,
			routed_provider, routing_reason, routing_original_model
		) VALUES (
			$1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18, $19, $20, $21, $22, $23, $24, $25, $26, $27, $28, $29, $30, $31, $32, $33, $34, $35, $36, $37, $38
		)`
	if skipExisting {
		query += " ON CONFLICT (id) DO NOTHING"
	}

	return db.ExecContext(ctx, query,
		log.ID, log.MajordomoAPIKeyID, log.ProviderAPIKeyHash, log.ProviderAPIKeyAlias,
		log.Provider, log.Model, log.RequestPath, log.RequestMethod,
		log.RequestedAt, log.RespondedAt, log.ResponseTimeMs,
		log.InputTokens, log.OutputTokens, log.CachedTokens, log.CacheCreationTokens,
		log.CacheCreation5mTokens, log.CacheCreation1hTokens,
		log.InputCost, log.CachedCost, log.CacheCreationCost, log.OutputCost, log.TotalCost,
		log.StatusCode, log.ErrorMessage, rawMetadataJSON, indexedMetadataJSON,
		log.BodyS3Key, log.ModelAliasFound,
		log.DeprecatedModelRedirected, log.OriginalModel,
		log.TraceID, log.SpanPath, log.SpanName, log.SpanID, log.ParentSpanID,
		log.RoutedProvider, log.RoutingReason, log.RoutingOriginalModel,
	)
}

// ForRequest resolves the metadata index before callers acquire a write transaction.
// Raw metadata is still persisted if the index lookup is unavailable.
func (c *ActiveKeysCache) ForRequest(ctx context.Context, id *uuid.UUID) map[string]bool {
	if id == nil || c == nil {
		return nil
	}
	keys, err := c.GetActiveKeys(ctx, *id)
	if err != nil {
		slog.Warn("load active metadata keys", "error", err)
	}
	return keys
}

// RegisterRequestMetadata records each metadata key and value seen, for discovery and
// cardinality, exactly as the request-log writer does for proxied requests.
func RegisterRequestMetadata(ctx context.Context, db *sqlx.DB, hll *HLLManager, logs []*models.RequestLog) {
	const query = `
		INSERT INTO llm_requests_metadata_keys (majordomo_api_key_id, key_name)
		VALUES ($1, $2)
		ON CONFLICT (majordomo_api_key_id, key_name) DO NOTHING`
	registered := map[string]bool{}
	for _, log := range logs {
		if log.MajordomoAPIKeyID == nil {
			continue
		}
		for k, v := range log.RawMetadata {
			if hll != nil {
				hll.AddValue(*log.MajordomoAPIKeyID, k, v)
			}
			if registered[k] {
				continue
			}
			registered[k] = true
			if _, err := db.ExecContext(ctx, query, *log.MajordomoAPIKeyID, k); err != nil {
				slog.Warn("register request metadata key", "key", k, "error", err)
			}
		}
	}
}
