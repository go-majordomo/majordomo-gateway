package controllers

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/go-chi/chi/v5"
	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/repositories"
	"github.com/go-majordomo/majordomo-gateway/internal/services"
)

// KeysController manages gateway API keys (mint / list / revoke). Keys are minted
// locally: the plaintext is returned once at creation and only the hash is stored.
type KeysController struct {
	svc *services.KeyService
}

// NewKeysController constructs a KeysController.
func NewKeysController(svc *services.KeyService) *KeysController {
	return &KeysController{svc: svc}
}

// RegisterRoutes mounts the key-management endpoints onto the given router.
func (c *KeysController) RegisterRoutes(r chi.Router) {
	r.Post("/keys", c.Create)
	r.Get("/keys", c.List)
	r.Delete("/keys/{id}", c.Revoke)
}

type createKeyRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description,omitempty"`
}

// createKeyResponse embeds the created key plus the plaintext, which is shown only once.
type createKeyResponse struct {
	*models.APIKey
	Key string `json:"key"`
}

func (c *KeysController) Create(w http.ResponseWriter, r *http.Request) {
	var req createKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		httputil.WriteJSONError(w, http.StatusBadRequest, "name is required")
		return
	}

	key, plaintext, err := c.svc.CreateKey(r.Context(), &models.CreateAPIKeyInput{
		Name:        req.Name,
		Description: req.Description,
	})
	if err != nil {
		slog.Error("failed to create api key", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}

	httputil.WriteJSON(w, http.StatusCreated, createKeyResponse{APIKey: key, Key: plaintext})
}

func (c *KeysController) List(w http.ResponseWriter, r *http.Request) {
	keys, err := c.svc.ListKeys(r.Context())
	if err != nil {
		slog.Error("failed to list api keys", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, keys)
}

func (c *KeysController) Revoke(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(chi.URLParam(r, "id"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid key ID")
		return
	}
	if err := c.svc.RevokeKey(r.Context(), id); err != nil {
		if errors.Is(err, repositories.ErrAPIKeyNotFound) {
			httputil.WriteJSONError(w, http.StatusNotFound, "key not found")
			return
		}
		slog.Error("failed to revoke api key", "error", err)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
