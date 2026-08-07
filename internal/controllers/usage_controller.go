package controllers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/storage"
)

// UsageController exposes read-only usage/reporting queries over HTTP. It reads
// directly from the usage repository — these are pure aggregations with no business
// logic, so no service layer sits in between.
type UsageController struct {
	usage     repositories.UsageStorage
	bodyStore storage.Store // nil when body archival is disabled
}

// NewUsageController constructs a UsageController. bodyStore may be nil.
func NewUsageController(usage repositories.UsageStorage, bodyStore storage.Store) *UsageController {
	return &UsageController{usage: usage, bodyStore: bodyStore}
}

// RegisterRoutes mounts the usage query endpoints onto the given router.
func (c *UsageController) RegisterRoutes(r chi.Router) {
	r.Post("/usage/summary", c.GetSummary)
	r.Post("/usage/daily", c.GetDailyUsage)
	r.Post("/usage/models", c.GetModelBreakdown)
	r.Post("/usage/providers", c.GetProviderBreakdown)
	r.Post("/usage/api-keys", c.GetAPIKeyBreakdown)
	r.Post("/usage/errors", c.GetErrorSummary)
	r.Post("/usage/errors/recent", c.GetRecentErrors)
	r.Post("/usage/latency", c.GetLatencyStats)
	r.Post("/usage/requests", c.ListRequests)
	r.Post("/usage/runs", c.ListRuns)
	r.Post("/usage/metadata/{keyName}", c.GetMetadataBreakdown)
	r.Get("/usage/runs/{traceId}", c.GetRunTree)
	r.Get("/usage/requests/{id}", c.GetRequestDetail)
	r.Get("/usage/requests/{id}/body", c.GetRequestBody)
}

func (c *UsageController) GetSummary(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := c.usage.GetUsageSummary(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get usage summary", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, summary)
}

func (c *UsageController) GetDailyUsage(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	daily, err := c.usage.GetDailyUsage(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get daily usage", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, daily)
}

func (c *UsageController) GetModelBreakdown(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.usage.GetModelBreakdown(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get model breakdown", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetProviderBreakdown(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.usage.GetProviderBreakdown(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get provider breakdown", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetAPIKeyBreakdown(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.usage.GetAPIKeyBreakdown(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get api key breakdown", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetErrorSummary(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := c.usage.GetErrorSummary(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get error summary", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, summary)
}

func (c *UsageController) GetRecentErrors(w http.ResponseWriter, r *http.Request) {
	filter, req, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(req.Limit, 20, 100)
	errorsList, err := c.usage.GetRecentErrors(r.Context(), filter, limit)
	if err != nil {
		slog.Error("failed to get recent errors", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, errorsList)
}

func (c *UsageController) GetLatencyStats(w http.ResponseWriter, r *http.Request) {
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := c.usage.GetLatencyStats(r.Context(), filter)
	if err != nil {
		slog.Error("failed to get latency stats", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, stats)
}

func (c *UsageController) ListRequests(w http.ResponseWriter, r *http.Request) {
	filter, req, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(req.Limit, 50, 200)
	offset := clampOffset(req.Offset)
	requests, total, err := c.usage.ListUsageRequests(r.Context(), filter, limit, offset)
	if err != nil {
		slog.Error("failed to list usage requests", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"requests":   requests,
		"numRecords": total,
	})
}

func (c *UsageController) ListRuns(w http.ResponseWriter, r *http.Request) {
	filter, req, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	limit := clampLimit(req.Limit, 50, 200)
	offset := clampOffset(req.Offset)
	runs, total, err := c.usage.ListRuns(r.Context(), filter, limit, offset)
	if err != nil {
		slog.Error("failed to list runs", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"runs":       runs,
		"numRecords": total,
	})
}

func (c *UsageController) GetMetadataBreakdown(w http.ResponseWriter, r *http.Request) {
	keyName := chi.URLParam(r, "keyName")
	if keyName == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "missing metadata key name")
		return
	}
	filter, _, err := decodeUsageRequest(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.usage.GetMetadataBreakdown(r.Context(), filter, keyName)
	if err != nil {
		slog.Error("failed to get metadata breakdown", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetRunTree(w http.ResponseWriter, r *http.Request) {
	traceID := chi.URLParam(r, "traceId")
	if traceID == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "missing trace id")
		return
	}
	// The run tree spans the whole history; use a wide default window.
	filter := &repositories.UsageFilter{}
	tree, err := c.usage.GetRunTree(r.Context(), filter, traceID)
	if err != nil {
		slog.Error("failed to get run tree", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if tree == nil {
		httputil.WriteJSONError(w, http.StatusNotFound, "run not found")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, tree)
}

func (c *UsageController) GetRequestDetail(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid request ID")
		return
	}
	detail, err := c.usage.GetRequestDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, repositories.ErrRequestNotFound) {
			httputil.WriteJSONError(w, http.StatusNotFound, "request not found")
			return
		}
		slog.Error("failed to get request detail", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, detail)
}

// GetRequestBody streams the archived request/response bodies for one request from
// object storage. 404 when body archival is disabled or nothing was archived.
func (c *UsageController) GetRequestBody(w http.ResponseWriter, r *http.Request) {
	if c.bodyStore == nil {
		httputil.WriteJSONError(w, http.StatusNotFound, "body archival is not enabled")
		return
	}
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid request ID")
		return
	}
	detail, err := c.usage.GetRequestDetail(r.Context(), id)
	if err != nil {
		if errors.Is(err, repositories.ErrRequestNotFound) {
			httputil.WriteJSONError(w, http.StatusNotFound, "request not found")
			return
		}
		slog.Error("failed to get request detail", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if detail.BodyS3Key == nil {
		httputil.WriteJSONError(w, http.StatusNotFound, "no body archived for this request")
		return
	}
	content, err := c.bodyStore.Download(r.Context(), *detail.BodyS3Key)
	if err != nil {
		slog.Error("failed to download archived body", "error", err, "key", *detail.BodyS3Key)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to fetch body")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, content)
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
