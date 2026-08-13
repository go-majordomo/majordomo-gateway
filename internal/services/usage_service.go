package services

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/storage"
)

// UsageService exposes usage/reporting queries. It owns the request→domain filter
// resolution (presets, date ranges, validation), pagination policy (limit/offset
// clamping), and the cross-store fetch of archived request bodies. Controllers hand it
// a transport-agnostic models.UsageQuery and receive domain results.
type UsageService struct {
	usage     repositories.UsageStorage
	bodyStore storage.Store // nil when body archival is disabled
}

// NewUsageService constructs a UsageService. bodyStore may be nil.
func NewUsageService(usage repositories.UsageStorage, bodyStore storage.Store) *UsageService {
	return &UsageService{usage: usage, bodyStore: bodyStore}
}

func (s *UsageService) Summary(ctx context.Context, q models.UsageQuery) (*models.UsageSummary, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetUsageSummary(ctx, filter)
}

func (s *UsageService) DailyUsage(ctx context.Context, q models.UsageQuery) ([]*models.DailyUsage, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetDailyUsage(ctx, filter)
}

func (s *UsageService) ModelBreakdown(ctx context.Context, q models.UsageQuery) ([]*models.ModelUsage, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetModelBreakdown(ctx, filter)
}

func (s *UsageService) ProviderBreakdown(ctx context.Context, q models.UsageQuery) ([]*models.ProviderUsage, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetProviderBreakdown(ctx, filter)
}

func (s *UsageService) APIKeyBreakdown(ctx context.Context, q models.UsageQuery) ([]*models.APIKeyUsage, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetAPIKeyBreakdown(ctx, filter)
}

func (s *UsageService) ErrorSummary(ctx context.Context, q models.UsageQuery) (*models.ErrorSummary, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetErrorSummary(ctx, filter)
}

func (s *UsageService) RecentErrors(ctx context.Context, q models.UsageQuery) ([]*models.RecentError, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	limit := clampLimit(q.Limit, 20, 100)
	return s.usage.GetRecentErrors(ctx, filter, limit)
}

func (s *UsageService) LatencyStats(ctx context.Context, q models.UsageQuery) (*models.LatencyStats, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetLatencyStats(ctx, filter)
}

func (s *UsageService) ListRequests(ctx context.Context, q models.UsageQuery) ([]*models.RequestListItem, int, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	offset := clampOffset(q.Offset)
	return s.usage.ListUsageRequests(ctx, filter, limit, offset)
}

func (s *UsageService) ListRuns(ctx context.Context, q models.UsageQuery) ([]*models.RunListItem, int, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, 0, err
	}
	limit := clampLimit(q.Limit, 50, 200)
	offset := clampOffset(q.Offset)
	return s.usage.ListRuns(ctx, filter, limit, offset)
}

func (s *UsageService) MetadataBreakdown(ctx context.Context, q models.UsageQuery, keyName string) ([]*models.MetadataBreakdown, error) {
	filter, err := resolveFilter(q)
	if err != nil {
		return nil, err
	}
	return s.usage.GetMetadataBreakdown(ctx, filter, keyName)
}

// RunTree returns the assembled waterfall for a single run. The tree spans the whole
// history, so a wide default window is used. Returns (nil, nil) when the run is not
// found.
func (s *UsageService) RunTree(ctx context.Context, traceID string) (*models.RunTree, error) {
	return s.usage.GetRunTree(ctx, &repositories.UsageFilter{}, traceID)
}

// RequestDetail returns a single request log. It returns
// repositories.ErrRequestNotFound when no such request exists.
func (s *UsageService) RequestDetail(ctx context.Context, id uuid.UUID) (*models.RequestLog, error) {
	return s.usage.GetRequestDetail(ctx, id)
}

// RequestBody streams the archived request/response bodies for one request from object
// storage. It returns ErrBodyArchivalDisabled when archival is off,
// repositories.ErrRequestNotFound when the request is unknown, and ErrNoBodyArchived
// when the request has no stored body.
func (s *UsageService) RequestBody(ctx context.Context, id uuid.UUID) (*storage.BodyContent, error) {
	if s.bodyStore == nil {
		return nil, ErrBodyArchivalDisabled
	}
	detail, err := s.usage.GetRequestDetail(ctx, id)
	if err != nil {
		return nil, err
	}
	if detail.BodyS3Key == nil {
		return nil, ErrNoBodyArchived
	}
	content, err := s.bodyStore.Download(ctx, *detail.BodyS3Key)
	if err != nil {
		return nil, fmt.Errorf("download archived body: %w", err)
	}
	return content, nil
}

// resolveFilter turns a transport-agnostic UsageQuery into a repository filter. The
// time range is taken from explicit start/end (YYYY-MM-DD or RFC3339) or a preset
// (7d/30d/90d, default 30d). At most two metadata filters are allowed. Validation
// failures are wrapped with ErrValidation.
func resolveFilter(q models.UsageQuery) (*repositories.UsageFilter, error) {
	if len(q.MetadataFilters) > 2 {
		return nil, validationErrorf("at most 2 metadata filters allowed")
	}

	filter := &repositories.UsageFilter{}

	if q.APIKeyID != "" {
		parsed, err := uuid.Parse(q.APIKeyID)
		if err != nil {
			return nil, validationErrorf("invalid api_key_id: %v", err)
		}
		filter.APIKeyID = &parsed
	}

	if q.Provider != "" {
		p := q.Provider
		filter.Provider = &p
	}
	if q.Model != "" {
		m := q.Model
		filter.Model = &m
	}

	switch q.StatusClass {
	case "", "error", "success":
		filter.StatusClass = q.StatusClass
	default:
		return nil, validationErrorf("invalid status_class: must be 'error' or 'success'")
	}

	filter.ExcludeRuns = q.ExcludeRuns

	for _, mf := range q.MetadataFilters {
		if mf.Key == "" || mf.Value == "" {
			return nil, validationErrorf("metadata filter key and value must be non-empty")
		}
		filter.MetadataFilters = append(filter.MetadataFilters, repositories.MetadataFilter{
			Key:   mf.Key,
			Value: mf.Value,
		})
	}

	if q.Start != "" {
		start, err := parseDate(q.Start)
		if err != nil {
			return nil, validationErrorf("invalid start date: %v", err)
		}
		filter.Start = start

		if q.End != "" {
			end, err := parseDate(q.End)
			if err != nil {
				return nil, validationErrorf("invalid end date: %v", err)
			}
			filter.End = end
		} else {
			filter.End = time.Now().UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
		}
	} else {
		preset := q.Preset
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

	return filter, nil
}

// parseDate parses YYYY-MM-DD or RFC3339.
func parseDate(s string) (time.Time, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, s)
}

func clampLimit(v, def, max int) int {
	if v <= 0 {
		return def
	}
	if v > max {
		return max
	}
	return v
}

func clampOffset(v int) int {
	if v < 0 {
		return 0
	}
	return v
}
