package repositories

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// ImportStorage is the interface satisfied by ImportRepository.
type ImportStorage interface {
	SaveImport(context.Context, []*models.RequestLog, []*models.ToolSpan) (int, int, error)
}

// ImportRepository writes usage imported from outside the proxy (e.g. Claude Code
// session transcripts) into the request log and the tool-span table. Unlike the
// async request-log writer it writes synchronously and is idempotent: imported
// rows carry deterministic ids, so re-importing a session skips rows already
// stored.
type ImportRepository struct {
	db             *sqlx.DB
	activeKeyCache *ActiveKeysCache
	hllManager     *HLLManager
}

// NewImportRepository constructs an ImportRepository.
func NewImportRepository(db *sqlx.DB, activeKeyCache *ActiveKeysCache, hllManager *HLLManager) *ImportRepository {
	return &ImportRepository{db: db, activeKeyCache: activeKeyCache, hllManager: hllManager}
}

// SaveImport commits the requests and their tool spans as one snapshot.
func (r *ImportRepository) SaveImport(ctx context.Context, logs []*models.RequestLog, spans []*models.ToolSpan) (int, int, error) {
	var active map[string]bool
	if len(logs) > 0 {
		active = r.activeKeyCache.ForRequest(ctx, logs[0].MajordomoAPIKeyID)
	}
	tx, err := r.db.BeginTxx(ctx, nil)
	if err != nil {
		return 0, 0, err
	}
	defer tx.Rollback() //nolint:errcheck
	var added []*models.RequestLog
	for _, log := range logs {
		result, err := InsertRequest(ctx, tx, active, log, true)
		if err != nil {
			return 0, 0, fmt.Errorf("insert imported request: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return 0, 0, err
		}
		if count > 0 {
			added = append(added, log)
		}
	}
	written, err := upsertToolSpans(ctx, tx, spans)
	if err != nil {
		return 0, 0, err
	}
	if err = tx.Commit(); err != nil {
		return 0, 0, err
	}
	RegisterRequestMetadata(ctx, r.db, r.hllManager, added)
	return len(added), written, nil
}

func upsertToolSpans(ctx context.Context, tx *sqlx.Tx, spans []*models.ToolSpan) (int, error) {
	const query = `
		INSERT INTO run_tool_spans (
			id, majordomo_api_key_id, trace_id, span_path, parent_span_id,
			tool_name, mcp_server, provider, model, called_at,
			result_tokens, reread_count, attributed_cost
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
		ON CONFLICT (id) DO UPDATE SET
			result_tokens = EXCLUDED.result_tokens,
			reread_count = EXCLUDED.reread_count,
			attributed_cost = EXCLUDED.attributed_cost,
			updated_at = now()
		WHERE (run_tool_spans.result_tokens,
		       run_tool_spans.reread_count, run_tool_spans.attributed_cost)
		      IS DISTINCT FROM
		      (EXCLUDED.result_tokens,
		       EXCLUDED.reread_count, EXCLUDED.attributed_cost)
		  AND run_tool_spans.majordomo_api_key_id IS NOT DISTINCT FROM EXCLUDED.majordomo_api_key_id`

	written := 0
	for _, s := range spans {
		res, err := tx.ExecContext(ctx, query,
			s.ID, s.MajordomoAPIKeyID, s.TraceID, s.SpanPath, s.ParentSpanID,
			s.ToolName, s.MCPServer, s.Provider, s.Model, s.CalledAt,
			s.ResultTokens, s.RereadCount, s.AttributedCost,
		)
		if err != nil {
			return 0, fmt.Errorf("upsert tool span %s: %w", s.ID, err)
		}
		n, _ := res.RowsAffected()
		written += int(n)
	}
	return written, nil
}
