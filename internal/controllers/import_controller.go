package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"

	"github.com/go-chi/chi/v5"

	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/services"
)

// maxImportBodyBytes bounds one import request body.
const maxImportBodyBytes = 32 << 20

// APIKeyResolver validates a gateway API key. Satisfied by *auth.Resolver.
type APIKeyResolver interface {
	ResolveAPIKey(ctx context.Context, apiKey string) (*models.APIKeyInfo, error)
}

// ImportController accepts usage recorded outside the proxy, such as Claude Code
// sessions run on a subscription. It authenticates with the caller's gateway API
// key (X-Majordomo-Key), like the proxy, not the admin token: anyone who can send
// traffic through the gateway can import their own usage, attributed to their key.
type ImportController struct {
	svc  *services.ImportService
	keys APIKeyResolver
}

// NewImportController constructs an ImportController.
func NewImportController(svc *services.ImportService, keys APIKeyResolver) *ImportController {
	return &ImportController{svc: svc, keys: keys}
}

// RegisterRoutes mounts the import endpoints onto the given router.
func (c *ImportController) RegisterRoutes(r chi.Router) {
	for _, source := range models.SessionSources() {
		r.Post("/v1/"+source+"/import", func(w http.ResponseWriter, r *http.Request) { c.importSession(w, r, source) })
	}
}

// ImportClaudeCode stores one Claude Code session's usage (token counts, model and
// tool names only) as request-log rows and tool spans in the run
// "claude-code:<key-id>:<session-id>".
func (c *ImportController) ImportClaudeCode(w http.ResponseWriter, r *http.Request) {
	c.importSession(w, r, "claude-code")
}

func (c *ImportController) importSession(w http.ResponseWriter, r *http.Request, source string) {
	key, err := c.keys.ResolveAPIKey(r.Context(), r.Header.Get("X-Majordomo-Key"))
	if err != nil {
		httputil.WriteJSONError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	var imp models.ClaudeCodeImport
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxImportBodyBytes)).Decode(&imp); err != nil {
		httputil.WriteJSONError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	result, err := c.svc.ImportSession(r.Context(), key.ID, source, &imp)
	if err != nil {
		if errors.Is(err, services.ErrValidation) {
			httputil.WriteJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		slog.Error("failed to import session", "error", err, "session_id", imp.SessionID)
		httputil.WriteJSONError(w, http.StatusInternalServerError, "internal error")
		return
	}
	httputil.WriteJSON(w, http.StatusOK, result)
}
