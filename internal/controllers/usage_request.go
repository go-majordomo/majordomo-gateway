package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// usageRequest is the JSON body accepted by the usage endpoints. It is bound here (an
// HTTP concern) and mapped to a transport-agnostic models.UsageQuery; the service
// resolves the time range, validates, and clamps pagination.
type usageRequest struct {
	Preset          string           `json:"preset"`
	Start           string           `json:"start"`
	End             string           `json:"end"`
	APIKeyID        string           `json:"api_key_id"`
	Provider        string           `json:"provider"`
	Model           string           `json:"model"`
	StatusClass     string           `json:"status_class"`
	MetadataFilters []metadataFilter `json:"metadata_filters"`
	ExcludeRuns     bool             `json:"exclude_runs"`
	Limit           int              `json:"limit"`
	Offset          int              `json:"offset"`
}

type metadataFilter struct {
	Key   string `json:"key"`
	Value string `json:"value"`
}

// decodeUsageQuery decodes a JSON body into a domain UsageQuery. An absent/empty body
// yields a zero-value query (the service applies defaults). Only malformed JSON is an
// error here; semantic validation happens in the service.
func decodeUsageQuery(r *http.Request) (models.UsageQuery, error) {
	var req usageRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return models.UsageQuery{}, fmt.Errorf("invalid JSON body: %w", err)
		}
	}

	q := models.UsageQuery{
		Preset:      req.Preset,
		Start:       req.Start,
		End:         req.End,
		APIKeyID:    req.APIKeyID,
		Provider:    req.Provider,
		Model:       req.Model,
		StatusClass: req.StatusClass,
		ExcludeRuns: req.ExcludeRuns,
		Limit:       req.Limit,
		Offset:      req.Offset,
	}
	for _, mf := range req.MetadataFilters {
		q.MetadataFilters = append(q.MetadataFilters, models.UsageQueryFilter{Key: mf.Key, Value: mf.Value})
	}
	return q, nil
}
