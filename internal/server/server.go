package server

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/controllers"
	"github.com/go-majordomo/majordomo-gateway/internal/httputil"
	"github.com/go-majordomo/majordomo-gateway/internal/proxy"
)

// HealthChecker can verify that a backing resource is reachable.
type HealthChecker interface {
	Ping(ctx context.Context) error
}

// Server wraps the HTTP server.
type Server struct {
	httpServer    *http.Server
	config        *config.ServerConfig
	healthChecker HealthChecker
}

// New builds and returns a fully configured Server. It wires the proxy handler as a
// catch-all at "/*" and exposes health/readiness probes. The admin/query API
// (/api/v1/keys, /api/v1/usage/*) is mounted, behind bearer-token auth, only when an
// admin token is configured.
func New(
	cfg *config.ServerConfig,
	proxyHandler *proxy.Handler,
	checker HealthChecker,
	adminToken string,
	keys *controllers.KeysController,
	usage *controllers.UsageController,
	metadata *controllers.MetadataController,
	providerKeys *controllers.ProviderKeysController,
) *Server {
	s := &Server{
		config:        cfg,
		healthChecker: checker,
	}

	router := chi.NewRouter()
	router.Use(Recovery)
	router.Use(RequestID)
	router.Use(Logger)

	router.Get("/health", healthHandler)
	router.Get("/readyz", s.readyzHandler)

	// Admin/query API — key management + usage reporting. Only mounted when an
	// ADMIN_TOKEN is configured; the CLI and MCP server are clients of these routes.
	if adminToken != "" && keys != nil && usage != nil && metadata != nil {
		router.Route("/api/v1", func(r chi.Router) {
			r.Use(adminAuthMiddleware(adminToken))
			keys.RegisterRoutes(r)
			usage.RegisterRoutes(r)
			metadata.RegisterRoutes(r)
			// Provider-key management is mounted only when server-side encryption
			// is configured (ENCRYPTION_KEY set); nil otherwise.
			if providerKeys != nil {
				providerKeys.RegisterRoutes(r)
			}
		})
		slog.Info("admin/query API enabled at /api/v1")
	}

	// Catch-all: forward every other request to the LLM proxy.
	router.Handle("/*", proxyHandler)

	s.httpServer = &http.Server{
		Addr:         fmt.Sprintf("%s:%d", cfg.Host, cfg.Port),
		Handler:      router,
		ReadTimeout:  cfg.ReadTimeout,
		WriteTimeout: cfg.WriteTimeout,
	}

	return s
}

// Start begins listening. It blocks until the server stops.
func (s *Server) Start() error {
	slog.Info("starting server", "addr", s.httpServer.Addr)
	return s.httpServer.ListenAndServe()
}

// Shutdown gracefully drains active connections within ctx.
func (s *Server) Shutdown(ctx context.Context) error {
	slog.Info("shutting down server")
	return s.httpServer.Shutdown(ctx)
}

// ShutdownWithTimeout is a convenience wrapper around Shutdown.
func (s *Server) ShutdownWithTimeout(timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	return s.Shutdown(ctx)
}

// adminAuthMiddleware rejects requests that don't carry the correct admin bearer token.
func adminAuthMiddleware(token string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			authHeader := r.Header.Get("Authorization")
			if authHeader == "" {
				httputil.WriteJSONError(w, http.StatusUnauthorized, "admin token required")
				return
			}
			if after, ok := strings.CutPrefix(authHeader, "Bearer "); !ok || strings.TrimSpace(after) != token {
				httputil.WriteJSONError(w, http.StatusForbidden, "invalid admin token")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func healthHandler(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("ok"))
}

func (s *Server) readyzHandler(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
	defer cancel()

	if err := s.healthChecker.Ping(ctx); err != nil {
		slog.Warn("readiness check failed", "error", err)
		httputil.WriteJSON(w, http.StatusServiceUnavailable, map[string]string{
			"status": "error",
			"error":  err.Error(),
		})
		return
	}

	httputil.WriteJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}
