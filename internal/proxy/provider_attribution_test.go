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
// format. Fireworks/Together/DeepSeek all parse with the OpenAI parser, which
// hardcodes "openai" — so before the fix a DeepSeek request logged as "openai".
func TestLoggedProviderReflectsRouting(t *testing.T) {
	// Fake upstream returns an OpenAI-format response, as OpenAI-compatible
	// providers (DeepSeek, Fireworks, Together) do.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"model":   "deepseek-chat",
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
			DeepSeek: config.ProviderConfig{BaseURL: upstream.URL},
		},
	}
	logs := &capturingLogWriter{ch: make(chan *models.RequestLog, 1)}
	h := NewHandler(logs, pricingSvc, newDeprecatedService(t, deprecatedModelJSON), resolver, cfg, nil)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(openAIRequest("deepseek-chat")))
	req.Header.Set("X-Majordomo-Key", "test-api-key")
	req.Header.Set("X-Majordomo-Provider", "deepseek")
	req.Header.Set("Content-Type", "application/json")

	h.ServeHTTP(httptest.NewRecorder(), req)

	select {
	case got := <-logs.ch:
		if got.Provider != "deepseek" {
			t.Errorf("logged Provider = %q, want %q (routing provider, not response format)", got.Provider, "deepseek")
		}
		if got.Model != "deepseek-chat" {
			t.Errorf("logged Model = %q, want %q", got.Model, "deepseek-chat")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for request log")
	}
}
