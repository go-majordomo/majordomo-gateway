package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/auth"
	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

// capturingLogWriter records the RequestLog the handler emits so tests can
// assert on it. WriteRequestLog runs in a background goroutine.
type capturingLogWriter struct{ ch chan *models.RequestLog }

func (c *capturingLogWriter) WriteRequestLog(_ context.Context, log *models.RequestLog) {
	c.ch <- log
}

// TestLoggedProviderReflectsRouting guards the fix that a request is attributed
// to the provider it was ROUTED to (X-Majordomo-Provider), not the response
// format. Fireworks/Together/Baseten all parse with the OpenAI parser, which
// hardcodes "openai" — so before the fix a Together request logged as "openai".
func TestLoggedProviderReflectsRouting(t *testing.T) {
	// Fake upstream returns an OpenAI-format response, as OpenAI-compatible
	// providers (Fireworks, Together, Baseten) do.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "zai-org/GLM-5.3",
			"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
			"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
		})
	}))
	defer upstream.Close()

	apiKey := &models.APIKey{ID: uuid.New(), KeyHash: auth.HashAPIKey("test-api-key"), IsActive: true}
	resolver := auth.NewResolver(&mockDeprecatedKeyStorage{key: apiKey})
	pricingSvc := pricing.NewService("", "", "", 24*time.Hour)
	cfg := &config.Config{
		Server: config.ServerConfig{UpstreamTimeout: 10 * time.Second, StreamHeaderTimeout: 5 * time.Second},
		Providers: config.ProvidersConfig{
			OpenAI:   config.ProviderConfig{BaseURL: upstream.URL},
			Together: config.ProviderConfig{BaseURL: upstream.URL},
		},
	}
	logs := &capturingLogWriter{ch: make(chan *models.RequestLog, 1)}
	h := NewHandler(logs, pricingSvc, newDeprecatedService(t, deprecatedModelJSON), resolver, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(openAIRequest("zai-org/GLM-5.3")))
	req.Header.Set("X-Majordomo-Key", "test-api-key")
	req.Header.Set("X-Majordomo-Provider", "together")
	req.Header.Set("Content-Type", "application/json")

	h.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case got := <-logs.ch:
		if got.Provider != "together" {
			t.Errorf("logged Provider = %q, want %q (routing provider, not response format)", got.Provider, "together")
		}
		if got.Model != "zai-org/GLM-5.3" {
			t.Errorf("logged Model = %q, want %q", got.Model, "zai-org/GLM-5.3")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request log")
	}
}

// TestUpstreamPathForOpenAICompatibleProviders asserts the path that actually
// reaches the upstream once the handler has composed base URL and path. The
// handler normalizes every OpenAI-compatible path to /v1/... on the way in, and
// these providers' base URLs stop before the version, so the upstream must see
// exactly one /v1 whether or not the client sent it, after any base URL prefix.
//
// gemini-openai is the inverse case (its base URL carries the version, so the
// /v1 is stripped back off) and shipped broken once — it sent
// /v1beta/openai/v1/chat/completions, which Google 404s. It has no config entry
// to point at a test server, so its composed URL is asserted by
// provider.TestUpstreamURLComposition instead.
func TestUpstreamPathForOpenAICompatibleProviders(t *testing.T) {
	tests := []struct {
		provider string
		// clientPath is what the caller sends; wantPath is what the upstream
		// must receive once baseURL and path are composed.
		clientPath string
		wantPath   string
	}{
		// Base URL with a path prefix before the version.
		{"fireworks", "/v1/chat/completions", "/inference/v1/chat/completions"},
		{"fireworks", "/chat/completions", "/inference/v1/chat/completions"},
		// Bare-origin base URL: the normalized /v1 must survive untouched.
		{"together", "/chat/completions", "/v1/chat/completions"},
		{"baseten", "/v1/chat/completions", "/v1/chat/completions"},
	}

	for _, tt := range tests {
		t.Run(tt.provider+tt.clientPath, func(t *testing.T) {
			gotPath := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath <- r.URL.Path
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model":   "m",
					"usage":   map[string]any{"prompt_tokens": 1, "completion_tokens": 1},
					"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
				})
			}))
			defer upstream.Close()

			// The provider's real base URL path prefix, appended to the test
			// server origin so the composed path is what the handler produced.
			prefix := map[string]string{
				"fireworks": "/inference",
				"together":  "",
				"baseten":   "",
			}[tt.provider]

			apiKey := &models.APIKey{ID: uuid.New(), KeyHash: auth.HashAPIKey("test-api-key"), IsActive: true}
			resolver := auth.NewResolver(&mockDeprecatedKeyStorage{key: apiKey})
			pricingSvc := pricing.NewService("", "", "", 24*time.Hour)
			providerCfg := config.ProviderConfig{BaseURL: upstream.URL + prefix}
			cfg := &config.Config{
				Server: config.ServerConfig{UpstreamTimeout: 10 * time.Second, StreamHeaderTimeout: 5 * time.Second},
			}
			switch tt.provider {
			case "fireworks":
				cfg.Providers.Fireworks = providerCfg
			case "together":
				cfg.Providers.Together = providerCfg
			case "baseten":
				cfg.Providers.Baseten = providerCfg
			}
			logs := &capturingLogWriter{ch: make(chan *models.RequestLog, 1)}
			h := NewHandler(logs, pricingSvc, newDeprecatedService(t, deprecatedModelJSON), resolver, cfg, nil)

			req := httptest.NewRequest(http.MethodPost, tt.clientPath, bytes.NewReader(openAIRequest("m")))
			req.Header.Set("X-Majordomo-Key", "test-api-key")
			req.Header.Set("X-Majordomo-Provider", tt.provider)
			req.Header.Set("Content-Type", "application/json")

			h.ServeHTTP(httptest.NewRecorder(), req)

			select {
			case got := <-gotPath:
				if got != tt.wantPath {
					t.Errorf("upstream received path %q, want %q", got, tt.wantPath)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for upstream request")
			}
		})
	}
}
