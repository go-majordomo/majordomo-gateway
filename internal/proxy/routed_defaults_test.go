package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/auth"
	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

// stubProviderKeys is a ProviderKeyResolver over provider -> plaintext key, stored
// in the form stubSecretStore decrypts.
type stubProviderKeys map[string]string

func (s stubProviderKeys) GetKey(_ context.Context, provider string) (*models.ProviderAPIKey, error) {
	pt, ok := s[provider]
	if !ok {
		return nil, errors.New("no provider key")
	}
	return &models.ProviderAPIKey{Provider: provider, EncryptedKey: "enc:" + pt}, nil
}

// stubSecretStore reverses the "enc:" prefix stubProviderKeys applies.
type stubSecretStore struct{}

func (stubSecretStore) Encrypt(plaintext string) (string, error) { return "enc:" + plaintext, nil }
func (stubSecretStore) Decrypt(ciphertext string) (string, error) {
	return strings.TrimPrefix(ciphertext, "enc:"), nil
}

// TestRoutedRequestAppliesEndpointDefaults sends a routed request through the
// handler and checks all three places an endpoint's request defaults surface:
// the body forwarded upstream, the X-Majordomo-Routed-Defaults response header,
// and the routing reason on the request log.
func TestRoutedRequestAppliesEndpointDefaults(t *testing.T) {
	tests := []struct {
		name         string
		body         string
		wantEffort   string // raw JSON forwarded upstream; "" means absent
		wantThinking bool
		wantApplied  string // applied default names; "" means none
	}{
		{
			name:        "default applied when the caller sets no reasoning field",
			body:        `{"model":"glm-5.3","messages":[{"role":"user","content":"hi"}]}`,
			wantEffort:  `"low"`,
			wantApplied: "reasoning_effort",
		},
		{
			name:       "caller's reasoning_effort is forwarded unchanged",
			body:       `{"model":"glm-5.3","reasoning_effort":"high","messages":[{"role":"user","content":"hi"}]}`,
			wantEffort: `"high"`,
		},
		{
			name:         "caller's thinking field withholds the reasoning_effort default",
			body:         `{"model":"glm-5.3","thinking":{"type":"enabled"},"messages":[{"role":"user","content":"hi"}]}`,
			wantThinking: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			forwarded := make(chan map[string]json.RawMessage, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				var fields map[string]json.RawMessage
				_ = json.Unmarshal(raw, &fields)
				forwarded <- fields
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(map[string]any{
					"model":   "tg/glm-5.3",
					"usage":   map[string]any{"prompt_tokens": 5, "completion_tokens": 7},
					"choices": []any{map[string]any{"message": map[string]any{"content": "hi"}}},
				})
			}))
			defer upstream.Close()

			apiKey := &models.APIKey{ID: uuid.New(), KeyHash: auth.HashAPIKey("test-api-key"), IsActive: true}
			resolver := auth.NewResolver(&mockDeprecatedKeyStorage{key: apiKey})
			pricingSvc := pricing.NewService("", "", "", 24*time.Hour)
			defer pricingSvc.Close()
			cfg := &config.Config{
				Server: config.ServerConfig{UpstreamTimeout: 10 * time.Second, StreamHeaderTimeout: 5 * time.Second},
			}

			endpoint := pricing.Endpoint{
				Provider:        "together",
				BaseURL:         upstream.URL,
				ProviderModelID: "tg/glm-5.3",
				RequestDefaults: map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"low"`)},
			}
			router := newRouter(fakeCatalog{"glm-5.3": {endpoint}}, fakeHealth{}, fakeKeys{"together": true}, fakePricer{}, 0)

			logs := &capturingLogWriter{ch: make(chan *models.RequestLog, 1)}
			h := NewHandler(logs, pricingSvc, newDeprecatedService(t, deprecatedModelJSON), resolver, cfg, nil,
				WithProviderRouting(router, stubProviderKeys{"together": "gateway-together"}, stubSecretStore{}))

			req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader([]byte(tt.body)))
			req.Header.Set("X-Majordomo-Key", "test-api-key")
			req.Header.Set("X-Majordomo-Provider", "majordomo")
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()

			h.ServeHTTP(rec, req)

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}

			var fields map[string]json.RawMessage
			select {
			case fields = <-forwarded:
			case <-time.After(2 * time.Second):
				t.Fatal("upstream never received the routed request")
			}
			if got := string(fields["model"]); got != `"tg/glm-5.3"` {
				t.Errorf("forwarded model = %s, want the provider-native id", got)
			}
			if got := string(fields["reasoning_effort"]); got != tt.wantEffort {
				t.Errorf("forwarded reasoning_effort = %q, want %q", got, tt.wantEffort)
			}
			if _, ok := fields["thinking"]; ok != tt.wantThinking {
				t.Errorf("forwarded thinking present = %v, want %v", ok, tt.wantThinking)
			}

			if got := rec.Header().Get("X-Majordomo-Routed-Defaults"); got != tt.wantApplied {
				t.Errorf("X-Majordomo-Routed-Defaults = %q, want %q", got, tt.wantApplied)
			}
			if got := rec.Header().Get("X-Majordomo-Routed-Model"); got != "tg/glm-5.3" {
				t.Errorf("X-Majordomo-Routed-Model = %q", got)
			}

			select {
			case got := <-logs.ch:
				if got.RoutingReason == nil {
					t.Fatal("request log has no routing reason")
				}
				reason := *got.RoutingReason
				if tt.wantApplied == "" {
					if strings.Contains(reason, "applied request defaults") {
						t.Errorf("routing reason %q reports defaults that were not applied", reason)
					}
				} else if want := "applied request defaults: " + tt.wantApplied; !strings.Contains(reason, want) {
					t.Errorf("routing reason %q does not contain %q", reason, want)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("timed out waiting for request log")
			}
		})
	}
}
