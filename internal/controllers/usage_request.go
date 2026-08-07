package controllers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

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

// decodeUsageRequest decodes a JSON body into a single-tenant UsageFilter. The time
// range is taken from explicit start/end (YYYY-MM-DD or RFC3339) or a preset
// (7d/30d/90d, default 30d). At most two metadata filters are allowed.
func decodeUsageRequest(r *http.Request) (*repositories.UsageFilter, *usageRequest, error) {
	var req usageRequest
	if r.Body != nil && r.ContentLength != 0 {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			return nil, nil, fmt.Errorf("invalid JSON body: %w", err)
		}
	}

	if len(req.MetadataFilters) > 2 {
		return nil, nil, fmt.Errorf("at most 2 metadata filters allowed")
	}

	filter := &repositories.UsageFilter{}

	if req.APIKeyID != "" {
		parsed, err := uuid.Parse(req.APIKeyID)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid api_key_id: %w", err)
		}
		filter.APIKeyID = &parsed
	}

	if req.Provider != "" {
		p := req.Provider
		filter.Provider = &p
	}
	if req.Model != "" {
		m := req.Model
		filter.Model = &m
	}

	switch req.StatusClass {
	case "", "error", "success":
		filter.StatusClass = req.StatusClass
	default:
		return nil, nil, fmt.Errorf("invalid status_class: must be 'error' or 'success'")
	}

	filter.ExcludeRuns = req.ExcludeRuns

	for _, mf := range req.MetadataFilters {
		if mf.Key == "" || mf.Value == "" {
			return nil, nil, fmt.Errorf("metadata filter key and value must be non-empty")
		}
		filter.MetadataFilters = append(filter.MetadataFilters, repositories.MetadataFilter{
			Key:   mf.Key,
			Value: mf.Value,
		})
	}

	if req.Start != "" {
		start, err := parseDate(req.Start)
		if err != nil {
			return nil, nil, fmt.Errorf("invalid start date: %w", err)
		}
		filter.Start = start

		if req.End != "" {
			end, err := parseDate(req.End)
			if err != nil {
				return nil, nil, fmt.Errorf("invalid end date: %w", err)
			}
			filter.End = end
		} else {
			filter.End = time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		}
	} else {
		preset := req.Preset
		if preset == "" {
			preset = "30d"
		}

		now := time.Now().UTC()
		filter.End = now.Truncate(24 * time.Hour).Add(24 * time.Hour)

		switch preset {
		case "7d":
			filter.Start = filter.End.AddDate(0, 0, -7)
		case "90d":
			filter.Start = filter.End.AddDate(0, 0, -90)
		default:
			filter.Start = filter.End.AddDate(0, 0, -30)
		}
	}

	return filter, &req, nil
}

// parseDate parses YYYY-MM-DD or RFC3339.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}
