package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
)

// backfillBatchSize bounds each retro-index / cleanup batch when a key is
// (de)activated, so the work never locks the table for long.
const backfillBatchSize = 1000

// MetadataController manages discovery and activation of request-metadata keys. Only
// activated keys are copied into indexed_metadata (and thus become queryable), which
// keeps the GIN index bounded regardless of what callers send.
type MetadataController struct {
	repo *repositories.MetadataKeyRepository
}

// NewMetadataController constructs a MetadataController.
func NewMetadataController(repo *repositories.MetadataKeyRepository) *MetadataController {
	return &MetadataController{repo: repo}
}

// RegisterRoutes mounts the metadata-key endpoints onto the given router.
func (c *MetadataController) RegisterRoutes(r chi.Router) {
	r.Get("/metadata", c.List)
	r.Post("/metadata/activate", c.Activate)
	r.Post("/metadata/deactivate", c.Deactivate)
}

// List returns every discovered metadata key with its approximate cardinality and
// whether it is currently indexed.
func (c *MetadataController) List(w http.ResponseWriter, r *http.Request) {
	keys, err := c.repo.ListMetadataKeys(r.Context())
	if err != nil {
		slog.Error("failed to list metadata keys", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, keys)
}

type metadataKeyRequest struct {
	APIKeyID string `json:"api_key_id"`
	KeyName  string `json:"key_name"`
}

func (c *MetadataController) decode(w http.ResponseWriter, r *http.Request) (uuid.UUID, string, bool) {
	var req metadataKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return uuid.Nil, "", false
	}
	apiKeyID, err := uuid.Parse(req.APIKeyID)
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid api_key_id")
		return uuid.Nil, "", false
	}
	if req.KeyName == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "key_name is required")
		return uuid.Nil, "", false
	}
	return apiKeyID, req.KeyName, true
}

// Activate marks a metadata key indexed for an API key and backfills existing rows
// asynchronously so past usage becomes queryable too.
func (c *MetadataController) Activate(w http.ResponseWriter, r *http.Request) {
	apiKeyID, keyName, ok := c.decode(w, r)
	if !ok {
		return
	}
	if err := c.repo.ActivateMetadataKey(r.Context(), apiKeyID, keyName); err != nil {
		c.mapErr(w, err, "activate metadata key")
		return
	}
	go c.backfill(apiKeyID, keyName)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "activated", "backfilling": true})
}

// Deactivate stops indexing a metadata key for an API key and removes it from existing
// indexed_metadata rows asynchronously.
func (c *MetadataController) Deactivate(w http.ResponseWriter, r *http.Request) {
	apiKeyID, keyName, ok := c.decode(w, r)
	if !ok {
		return
	}
	if err := c.repo.DeactivateMetadataKey(r.Context(), apiKeyID, keyName); err != nil {
		c.mapErr(w, err, "deactivate metadata key")
		return
	}
	go c.cleanup(apiKeyID, keyName)
	httputil.WriteJSON(w, http.StatusOK, map[string]any{"status": "deactivated", "cleaning": true})
}

func (c *MetadataController) backfill(apiKeyID uuid.UUID, keyName string) {
	if _, err := c.repo.BackfillIndexedMetadata(context.Background(), apiKeyID, keyName, backfillBatchSize); err != nil {
		slog.Error("metadata backfill failed", "error", err, "api_key_id", apiKeyID, "key", keyName)
	}
}

func (c *MetadataController) cleanup(apiKeyID uuid.UUID, keyName string) {
	if _, err := c.repo.CleanupIndexedMetadata(context.Background(), apiKeyID, keyName, backfillBatchSize); err != nil {
		slog.Error("metadata cleanup failed", "error", err, "api_key_id", apiKeyID, "key", keyName)
	}
}

func (c *MetadataController) mapErr(w http.ResponseWriter, err error, msg string) {
	if errors.Is(err, repositories.ErrMetadataKeyNotFound) {
		httputil.WriteJSONError(w, http.StatusNotFound, "metadata key not found for that API key")
		return
	}
	slog.Error("failed to "+msg, "error", err)
	httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
}
