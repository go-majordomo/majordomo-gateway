// Package requestlog provides async writing of request logs to PostgreSQL.
package requestlog

import (
	"context"
	"encoding/json"
	"log/slog"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// Writer asynchronously writes request logs to PostgreSQL. It records every metadata
// key it sees (feeding a HyperLogLog cardinality estimate) but only copies *activated*
// keys into indexed_metadata, so the GIN index stays bounded no matter what callers send.
type Writer struct {
	db             *sqlx.DB
	logChan        chan *models.RequestLog
	done           chan struct{}
	activeKeyCache *repositories.ActiveKeysCache
	hllManager     *repositories.HLLManager
}

// New constructs a Writer and starts the background write loop.
func New(db *sqlx.DB, activeKeyCache *repositories.ActiveKeysCache, hllManager *repositories.HLLManager) *Writer {
	w := &Writer{
		db:             db,
		logChan:        make(chan *models.RequestLog, 1000),
		done:           make(chan struct{}),
		activeKeyCache: activeKeyCache,
		hllManager:     hllManager,
	}
	go w.writeLoop()
	return w
}

// WriteRequestLog enqueues a request log for async writing. Non-blocking: drops
// the log with a warning if the channel is full.
func (w *Writer) WriteRequestLog(_ context.Context, log *models.RequestLog) {
	select {
	case w.logChan <- log:
	default:
		slog.Warn("request log channel full, dropping log", "request_id", log.ID)
	}
}

// Ping verifies database connectivity. Satisfies server.HealthChecker.
func (w *Writer) Ping(ctx context.Context) error {
	return w.db.PingContext(ctx)
}

// Close drains the write channel, flushes HLL state, and closes the database.
func (w *Writer) Close() error {
	close(w.done)
	if w.hllManager != nil {
		w.hllManager.Stop()
	}
	return w.db.Close()
}

func (w *Writer) writeLoop() {
	for {
		select {
		case log := <-w.logChan:
			w.writeLog(log)
		case <-w.done:
			for len(w.logChan) > 0 {
				w.writeLog(<-w.logChan)
			}
			return
		}
	}
}

func (w *Writer) writeLog(log *models.RequestLog) {
	ctx := context.Background()

	// Copy only *active* metadata keys into indexed_metadata; the rest live in
	// raw_metadata only. This keeps the indexed_metadata GIN index bounded.
	indexedMetadata := make(map[string]string)
	if log.MajordomoAPIKeyID != nil {
		activeKeys, _ := w.activeKeyCache.GetActiveKeys(ctx, *log.MajordomoAPIKeyID)
		for key, value := range log.RawMetadata {
			if activeKeys[key] {
				indexedMetadata[key] = value
			}
		}
	}

	rawMetadataJSON, err := json.Marshal(log.RawMetadata)
	if err != nil {
		slog.Error("failed to marshal raw metadata", "error", err)
		rawMetadataJSON = []byte("{}")
	}
	indexedMetadataJSON, err := json.Marshal(indexedMetadata)
	if err != nil {
		slog.Error("failed to marshal indexed metadata", "error", err)
		indexedMetadataJSON = []byte("{}")
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

	_, err = w.db.ExecContext(ctx, query,
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
	if err != nil {
		slog.Error("failed to write request log", "error", err, "request_id", log.ID)
		return
	}

	// Record every metadata key seen (for discovery + cardinality), regardless of
	// whether it is currently indexed.
	if log.MajordomoAPIKeyID != nil {
		for key, value := range log.RawMetadata {
			w.hllManager.AddValue(*log.MajordomoAPIKeyID, key, value)
		}
		w.registerMetadataKeys(ctx, *log.MajordomoAPIKeyID, log.RawMetadata)
	}
}

func (w *Writer) registerMetadataKeys(ctx context.Context, apiKeyID uuid.UUID, metadata map[string]string) {
	if len(metadata) == 0 {
		return
	}

	// Ensure a row exists for each discovered key. request_count / last_seen_at /
	// approx_cardinality are maintained by the HLL manager's periodic flush, so this
	// only needs to register the key's existence.
	query := `
		INSERT INTO llm_requests_metadata_keys (majordomo_api_key_id, key_name)
		VALUES ($1, $2)
		ON CONFLICT (majordomo_api_key_id, key_name) DO NOTHING`

	for key := range metadata {
		if _, err := w.db.ExecContext(ctx, query, apiKeyID, key); err != nil {
			slog.Warn("failed to register metadata key", "error", err, "key", key)
		}
	}
}
