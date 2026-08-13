package repositories

import (
	"context"
	"fmt"
	"time"

	"github.com/jmoiron/sqlx"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// EndpointHealthRepository computes recent health aggregates per (provider, model)
// from the request log. It backs the provider router's health gate — the signal
// is derived from data the gateway already writes, not a separate capture path.
type EndpointHealthRepository struct {
	db *sqlx.DB
}

// NewEndpointHealthRepository constructs an EndpointHealthRepository backed by the given database.
func NewEndpointHealthRepository(db *sqlx.DB) *EndpointHealthRepository {
	return &EndpointHealthRepository{db: db}
}

// GetEndpointHealth returns per-(provider, model) health aggregates over requests
// logged after `since`: sample count, average latency, and the fraction of
// responses with a 5xx status. Rows with no recent traffic simply do not appear.
func (r *EndpointHealthRepository) GetEndpointHealth(ctx context.Context, since time.Time) ([]models.EndpointHealth, error) {
	const query = `
		SELECT
			provider,
			model,
			count(*)                                                     AS sample_count,
			coalesce(avg(response_time_ms), 0)                           AS avg_latency_ms,
			count(*) FILTER (WHERE status_code >= 500)::float8 / count(*) AS error_rate
		FROM llm_requests
		WHERE requested_at > $1
		GROUP BY provider, model`

	var health []models.EndpointHealth
	if err := r.db.SelectContext(ctx, &health, query, since); err != nil {
		return nil, fmt.Errorf("get endpoint health: %w", err)
	}
	return health, nil
}
