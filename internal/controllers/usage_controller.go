package controllers

import (
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/services"
)

// UsageController exposes usage/reporting queries over HTTP. It binds request bodies,
// delegates to the usage service (which owns filter resolution, pagination policy, and
// the archived-body fetch), and maps domain errors to HTTP status codes.
type UsageController struct {
	svc *services.UsageService
}

// NewUsageController constructs a UsageController.
func NewUsageController(svc *services.UsageService) *UsageController {
	return &UsageController{svc: svc}
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
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := c.svc.Summary(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get usage summary")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, summary)
}

func (c *UsageController) GetDailyUsage(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	daily, err := c.svc.DailyUsage(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get daily usage")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, daily)
}

func (c *UsageController) GetModelBreakdown(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.svc.ModelBreakdown(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get model breakdown")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetProviderBreakdown(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.svc.ProviderBreakdown(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get provider breakdown")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetAPIKeyBreakdown(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.svc.APIKeyBreakdown(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get api key breakdown")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, breakdown)
}

func (c *UsageController) GetErrorSummary(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	summary, err := c.svc.ErrorSummary(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get error summary")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, summary)
}

func (c *UsageController) GetRecentErrors(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	errorsList, err := c.svc.RecentErrors(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get recent errors")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, errorsList)
}

func (c *UsageController) GetLatencyStats(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	stats, err := c.svc.LatencyStats(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "get latency stats")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, stats)
}

func (c *UsageController) ListRequests(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	requests, total, err := c.svc.ListRequests(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "list usage requests")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, map[string]interface{}{
		"requests":   requests,
		"numRecords": total,
	})
}

func (c *UsageController) ListRuns(w http.ResponseWriter, r *http.Request) {
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	runs, total, err := c.svc.ListRuns(r.Context(), q)
	if err != nil {
		c.writeErr(w, err, "list runs")
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
	q, err := decodeUsageQuery(r)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	breakdown, err := c.svc.MetadataBreakdown(r.Context(), q, keyName)
	if err != nil {
		c.writeErr(w, err, "get metadata breakdown")
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
	tree, err := c.svc.RunTree(r.Context(), traceID)
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
	detail, err := c.svc.RequestDetail(r.Context(), id)
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
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid request ID")
		return
	}
	content, err := c.svc.RequestBody(r.Context(), id)
	if err != nil {
		switch {
		case errors.Is(err, services.ErrBodyArchivalDisabled):
			httputil.WriteJSONError(w, http.StatusNotFound, "body archival is not enabled")
		case errors.Is(err, repositories.ErrRequestNotFound):
			httputil.WriteJSONError(w, http.StatusNotFound, "request not found")
		case errors.Is(err, services.ErrNoBodyArchived):
			httputil.WriteJSONError(w, http.StatusNotFound, "no body archived for this request")
		default:
			slog.Error("failed to fetch archived body", "error", err, "request_id", id)
			httputil.WriteJSONError(w, http.StatusInternalServerError, "failed to fetch body")
		}
		return
	}
	httputil.WriteJSON(w, http.StatusOK, content)
}

// writeErr maps a usage-service error to an HTTP response: validation failures become
// 400 with the client-safe message, everything else a logged 500.
func (c *UsageController) writeErr(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, services.ErrValidation) {
		httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	slog.Error("failed to "+msg, "error", err)
	httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
}
