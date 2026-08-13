package controllers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/provider"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/secrets"
)

// ProviderKeysController manages stored upstream provider credentials used by
// provider routing. Keys are POSTed in plaintext, encrypted server-side with the
// gateway's ENCRYPTION_KEY, and never returned by the API.
type ProviderKeysController struct {
	repo    repositories.ProviderKeyStorage
	secrets secrets.SecretStore
}

// NewProviderKeysController constructs a ProviderKeysController.
func NewProviderKeysController(repo repositories.ProviderKeyStorage, secrets secrets.SecretStore) *ProviderKeysController {
	return &ProviderKeysController{repo: repo, secrets: secrets}
}

// RegisterRoutes mounts the provider-key management endpoints onto the given router.
func (c *ProviderKeysController) RegisterRoutes(r chi.Router) {
	r.Post("/provider-keys", c.Upsert)
	r.Get("/provider-keys", c.List)
	r.Delete("/provider-keys/{provider}", c.Delete)
}

type upsertProviderKeyRequest struct {
	Provider string `json:"provider"`
	Key      string `json:"key"`
}

// upsertProviderKeyResponse acknowledges storage without echoing the key material.
type upsertProviderKeyResponse struct {
	Provider string `json:"provider"`
}

func (c *ProviderKeysController) Upsert(w http.ResponseWriter, r *http.Request) {
	var req upsertProviderKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Provider = strings.ToLower(strings.TrimSpace(req.Provider))
	req.Key = strings.TrimSpace(req.Key)
	if req.Provider == "" || req.Key == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "provider and key are required")
		return
	}
	if !provider.IsCredentialProvider(req.Provider) {
		httputil.WriteJSONError(w, http.StatusBadRequest, "unknown provider: "+req.Provider)
		return
	}

	encrypted, err := c.secrets.Encrypt(req.Key)
	if err != nil {
		slog.Error("failed to encrypt provider key", "error", err, "provider", req.Provider)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	if err := c.repo.Upsert(r.Context(), req.Provider, encrypted); err != nil {
		slog.Error("failed to store provider key", "error", err, "provider", req.Provider)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	// Never echo the key material.
	httputil.WriteJSON(w, http.StatusCreated, upsertProviderKeyResponse{Provider: req.Provider})
}

func (c *ProviderKeysController) List(w http.ResponseWriter, r *http.Request) {
	keys, err := c.repo.List(r.Context())
	if err != nil {
		slog.Error("failed to list provider keys", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, keys)
}

func (c *ProviderKeysController) Delete(w http.ResponseWriter, r *http.Request) {
	prov := strings.ToLower(strings.TrimSpace(chi.URLParam(r, "provider")))
	if prov == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "provider is required")
		return
	}
	if err := c.repo.Delete(r.Context(), prov); err != nil {
		if errors.Is(err, repositories.ErrProviderKeyNotFound) {
			httputil.WriteJSONError(w, http.StatusNotFound, "provider key not found")
			return
		}
		slog.Error("failed to delete provider key", "error", err, "provider", prov)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
