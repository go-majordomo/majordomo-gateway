package pricing

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// TestShippedModelCatalog loads the real model_catalog.json through the loader
// and guards the properties routing + attribution depend on: per-provider prices
// are distinct for a shared id, every routable endpoint is priced with a
// /v1-less base URL, and a non-routable model is priced but not routable.
func TestShippedModelCatalog(t *testing.T) {
	data, err := os.ReadFile("../../model_catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	pricing, byProvider, catalog, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("shipped model_catalog.json failed to parse: %v", err)
	}
	s := &Service{pricing: pricing, byProvider: byProvider, catalog: catalog}

	// Per-provider pricing: a shared HF id resolves to distinct prices per provider.
	tg, ok := s.resolve("together", "moonshotai/Kimi-K3")
	if !ok {
		t.Fatal("together price for shared id missing")
	}
	bt, ok := s.resolve("baseten", "moonshotai/Kimi-K3")
	if !ok {
		t.Fatal("baseten price for shared id missing")
	}
	if tg.InputPricePerMillion == bt.InputPricePerMillion {
		t.Errorf("expected distinct per-provider prices for a shared id; both = %v", tg.InputPricePerMillion)
	}

	// Routing catalog: glm-5.2 is routable across >=2 providers; every candidate is
	// well-formed and priceable by (provider, model).
	eps, ok := s.Candidates("glm-5.2")
	if !ok || len(eps) < 2 {
		t.Fatalf("expected glm-5.2 routable with >=2 endpoints, got ok=%v n=%d", ok, len(eps))
	}
	for _, e := range eps {
		if e.Provider == "" || e.BaseURL == "" || e.ProviderModelID == "" {
			t.Errorf("endpoint missing fields: %+v", e)
		}
		if strings.HasSuffix(e.BaseURL, "/v1") {
			t.Errorf("%s base_url must omit /v1 (NormalizeOpenAIPath adds it): %q", e.Provider, e.BaseURL)
		}
		if _, ok := s.resolve(e.Provider, e.ProviderModelID); !ok {
			t.Errorf("routed endpoint %s/%s has no price", e.Provider, e.ProviderModelID)
		}
	}

	// Every routable slug offers a real choice: at least two endpoints.
	for slug, eps := range catalog {
		if len(eps) < 2 {
			t.Errorf("route %q has %d endpoint(s), want >= 2", slug, len(eps))
		}
	}

	// No route entry was dropped by the loader (unknown provider or model, or a
	// request default that sets the model), and no default can override the model.
	var raw catalogFile
	if err := json.Unmarshal(data, &raw); err != nil {
		t.Fatal(err)
	}
	for slug, entries := range raw.Routes {
		if len(catalog[slug]) != len(entries) {
			t.Errorf("route %q: loader kept %d of %d entries", slug, len(catalog[slug]), len(entries))
		}
	}

	// Request defaults are per endpoint: glm-5.3 lowers reasoning on every
	// provider, while Baseten's glm-5.2 must carry none (reasoning is opt-in
	// there, so any reasoning_effort would raise its cost).
	for _, e := range mustCandidates(t, s, "glm-5.3") {
		if got := string(e.RequestDefaults["reasoning_effort"]); got != `"low"` {
			t.Errorf("glm-5.3 on %s: reasoning_effort default = %s, want \"low\"", e.Provider, got)
		}
	}
	for _, e := range mustCandidates(t, s, "glm-5.2") {
		if len(e.RequestDefaults) != 0 {
			t.Errorf("glm-5.2 on %s: unexpected request defaults %v", e.Provider, e.RequestDefaults)
		}
	}

	// A non-routable model is priced (attribution) but carries no route.
	if _, ok := s.resolve("openai", "gpt-5"); !ok {
		t.Error("gpt-5 price missing")
	}
	if _, ok := s.Candidates("gpt-5"); ok {
		t.Error("gpt-5 should not be routable")
	}
}

func TestCalculate_CacheCreationTTL(t *testing.T) {
	s := &Service{
		pricing: map[string]ModelPricing{
			"claude-sonnet-4-5": {
				InputPricePerMillion:  3,
				OutputPricePerMillion: 15,
				CachedPricePerMillion: 0.3,
			},
		},
	}

	// InputTokens is the normalized total (fresh + cache-read + cache-write).
	metrics := &models.UsageMetrics{
		Model:                 "claude-sonnet-4-5",
		InputTokens:           10000,
		OutputTokens:          500,
		CachedTokens:          1800,
		CacheCreationTokens:   248,
		CacheCreation5mTokens: 148,
		CacheCreation1hTokens: 100,
	}

	cost := s.Calculate(metrics)

	if !cost.ModelAliasFound {
		t.Fatal("ModelAliasFound = false, want true")
	}

	const perMillion = 1_000_000.0
	freshInput := float64(10000-1800-248) * 3 / perMillion
	wantCached := float64(1800) * 0.3 / perMillion
	// 5m writes bill at 1.25x input, 1h writes at 2x input.
	wantCacheCreation := (148*1.25 + 100*2.0) * 3 / perMillion
	wantOutput := float64(500) * 15 / perMillion
	wantTotal := freshInput + wantCached + wantCacheCreation + wantOutput

	assertClose(t, "InputCost", cost.InputCost, freshInput)
	assertClose(t, "CachedCost", cost.CachedCost, wantCached)
	assertClose(t, "CacheCreationCost", cost.CacheCreationCost, wantCacheCreation)
	assertClose(t, "OutputCost", cost.OutputCost, wantOutput)
	assertClose(t, "TotalCost", cost.TotalCost, wantTotal)
}

// A provider (e.g. Bedrock) may report a cache-creation total with no per-TTL
// breakdown. The whole amount must be priced at the 5-minute rate rather than
// dropped from the input bucket at zero cost.
func TestCalculate_CacheCreationWithoutTTLBreakdown(t *testing.T) {
	s := &Service{
		pricing: map[string]ModelPricing{
			"claude-on-bedrock": {InputPricePerMillion: 3, OutputPricePerMillion: 15},
		},
	}

	metrics := &models.UsageMetrics{
		Model:               "claude-on-bedrock",
		InputTokens:         5000,
		CacheCreationTokens: 4000,
		// No 5m/1h split (Bedrock convention via the pricing-side fallback).
	}

	cost := s.Calculate(metrics)

	const perMillion = 1_000_000.0
	wantCacheCreation := 4000 * 1.25 * 3 / perMillion // all attributed to 5m
	wantInput := float64(5000-4000) * 3 / perMillion
	assertClose(t, "CacheCreationCost", cost.CacheCreationCost, wantCacheCreation)
	assertClose(t, "InputCost", cost.InputCost, wantInput)
	if cost.CacheCreationCost == 0 {
		t.Error("cache-creation tokens were dropped at zero cost")
	}
}

func TestCalculate_UnknownModel(t *testing.T) {
	s := &Service{pricing: map[string]ModelPricing{}}
	cost := s.Calculate(&models.UsageMetrics{Model: "does-not-exist"})
	if cost.ModelAliasFound {
		t.Error("ModelAliasFound = true, want false for unknown model")
	}
	if cost.TotalCost != 0 {
		t.Errorf("TotalCost = %v, want 0 for unknown model", cost.TotalCost)
	}
}

func assertClose(t *testing.T, name string, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-12 {
		t.Errorf("%s = %v, want %v", name, got, want)
	}
}

// testCatalogJSON is a minimal local catalog: one model that the remote feed
// also prices (so precedence is observable) and one route.
const testCatalogJSON = `{
  "providers": {
    "openai": {
      "base_url": "https://api.openai.com",
      "models": {
        "shared-model": {"input_price_per_million": 2, "output_price_per_million": 10, "cached_price_per_million": 0.2}
      }
    },
    "together": {
      "base_url": "https://api.together.xyz",
      "models": {
        "org/routed": {"input_price_per_million": 1, "output_price_per_million": 4, "cached_price_per_million": 0}
      }
    },
    "baseten": {
      "base_url": "https://inference.baseten.co",
      "models": {
        "org/routed": {"input_price_per_million": 1.5, "output_price_per_million": 4, "cached_price_per_million": 0}
      }
    }
  },
  "routes": {
    "routed": [
      {"provider": "together", "model": "org/routed"},
      {"provider": "baseten", "model": "org/routed"}
    ]
  }
}`

// remoteFeed serves a price feed that disagrees with testCatalogJSON on
// shared-model and adds remote-only-model. Setting fail makes it return 500.
func remoteFeed(t *testing.T, fail *atomic.Bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if fail != nil && fail.Load() {
			http.Error(w, "unavailable", http.StatusInternalServerError)
			return
		}
		cached := 9.9
		_ = json.NewEncoder(w).Encode(remotePricingResponse{
			UpdatedAt: "2026-10-11",
			Prices: []remotePriceEntry{
				{ID: "shared-model", Vendor: "openai", Input: 99, Output: 999, InputCached: &cached},
				{ID: "remote-only-model", Vendor: "openai", Input: 7, Output: 70},
			},
		})
	}))
	t.Cleanup(srv.Close)
	return srv
}

func writeTestCatalog(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "model_catalog.json")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func newTestService(t *testing.T, remoteURL, catalogFile string) *Service {
	t.Helper()
	s := NewService(remoteURL, catalogFile, "", time.Hour)
	t.Cleanup(s.Close)
	return s
}

func TestReload_LocalCatalogOverridesRemote(t *testing.T) {
	s := newTestService(t, remoteFeed(t, nil).URL, writeTestCatalog(t, testCatalogJSON))

	for _, provider := range []string{"openai", ""} {
		p, ok := s.Price(provider, "shared-model")
		if !ok {
			t.Fatalf("provider=%q: shared-model unpriced", provider)
		}
		if p.InputPricePerMillion != 2 || p.OutputPricePerMillion != 10 || p.CachedPricePerMillion != 0.2 {
			t.Errorf("provider=%q: shared-model = %+v, want the local catalog price", provider, p)
		}
	}

	// The remote feed still supplies models the catalog does not list.
	if p, ok := s.Price("openai", "remote-only-model"); !ok || p.InputPricePerMillion != 7 {
		t.Errorf("remote-only-model = %+v ok=%v, want the remote price", p, ok)
	}

	if eps, ok := s.Candidates("routed"); !ok || len(eps) != 2 {
		t.Errorf("routed candidates = %d ok=%v, want 2", len(eps), ok)
	}
}

func TestReload_RemoteUnavailableAtStartupUsesCatalog(t *testing.T) {
	var fail atomic.Bool
	fail.Store(true)
	s := newTestService(t, remoteFeed(t, &fail).URL, writeTestCatalog(t, testCatalogJSON))

	if p, ok := s.Price("openai", "shared-model"); !ok || p.InputPricePerMillion != 2 {
		t.Errorf("shared-model = %+v ok=%v, want the local catalog price", p, ok)
	}
	if _, ok := s.Price("openai", "remote-only-model"); ok {
		t.Error("remote-only-model priced although the remote feed was unavailable")
	}
	if _, ok := s.Candidates("routed"); !ok {
		t.Error("routes must load from the catalog without the remote feed")
	}
}

func TestReload_RefreshFailureKeepsCurrentState(t *testing.T) {
	var fail atomic.Bool
	s := newTestService(t, remoteFeed(t, &fail).URL, writeTestCatalog(t, testCatalogJSON))

	fail.Store(true)
	if err := s.reload(false); err == nil {
		t.Fatal("expected refresh to report the remote failure")
	}

	if p, ok := s.Price("openai", "remote-only-model"); !ok || p.InputPricePerMillion != 7 {
		t.Errorf("remote-only-model = %+v ok=%v, want it retained after a failed refresh", p, ok)
	}
	if p, ok := s.Price("openai", "shared-model"); !ok || p.InputPricePerMillion != 2 {
		t.Errorf("shared-model = %+v ok=%v, want the local catalog price", p, ok)
	}
}

func TestReload_CatalogUnreadableUsesRemoteAndKeepsRoutes(t *testing.T) {
	catalogFile := writeTestCatalog(t, testCatalogJSON)
	s := newTestService(t, remoteFeed(t, nil).URL, catalogFile)

	if err := os.WriteFile(catalogFile, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := s.reload(false); err != nil {
		t.Fatalf("reload with a readable remote feed: %v", err)
	}

	if p, ok := s.Price("openai", "shared-model"); !ok || p.InputPricePerMillion != 99 {
		t.Errorf("shared-model = %+v ok=%v, want the remote price once the catalog is unreadable", p, ok)
	}
	if _, ok := s.Candidates("routed"); !ok {
		t.Error("previously loaded routes must survive an unreadable catalog")
	}
}

func TestReload_NoSourceReturnsError(t *testing.T) {
	s := &Service{catalogFile: filepath.Join(t.TempDir(), "missing.json")}
	if err := s.reload(true); err == nil {
		t.Error("expected an error when neither the catalog nor a remote feed is available")
	}
}

func mustCandidates(t *testing.T, s *Service, slug string) []Endpoint {
	t.Helper()
	eps, ok := s.Candidates(slug)
	if !ok {
		t.Fatalf("%s is not routable", slug)
	}
	return eps
}

func TestParseCatalog_RequestDefaults(t *testing.T) {
	const catalogJSON = `{
  "providers": {
    "together": {"base_url": "https://api.together.xyz", "models": {"org/m": {"input_price_per_million": 1, "output_price_per_million": 2}}},
    "baseten": {"base_url": "https://inference.baseten.co", "models": {"org/m": {"input_price_per_million": 1, "output_price_per_million": 2}}},
    "fireworks": {"base_url": "https://api.fireworks.ai/inference", "models": {"fw/m": {"input_price_per_million": 1, "output_price_per_million": 2}}}
  },
  "routes": {
    "m": [
      {"provider": "together", "model": "org/m", "request_defaults": {"reasoning_effort": "low", "thinking": {"type": "disabled"}}},
      {"provider": "baseten", "model": "org/m"},
      {"provider": "fireworks", "model": "fw/m", "request_defaults": {"model": "something-else"}}
    ]
  }
}`
	_, _, catalog, err := parseCatalog([]byte(catalogJSON))
	if err != nil {
		t.Fatal(err)
	}

	eps := catalog["m"]
	if len(eps) != 2 {
		t.Fatalf("got %d endpoints, want 2: the entry whose defaults set the model must be dropped", len(eps))
	}
	byProvider := map[string]Endpoint{}
	for _, e := range eps {
		byProvider[e.Provider] = e
	}
	if _, ok := byProvider["fireworks"]; ok {
		t.Error("an entry whose request_defaults set the model must not be routable")
	}

	together := byProvider["together"].RequestDefaults
	if got := string(together["reasoning_effort"]); got != `"low"` {
		t.Errorf("reasoning_effort = %s, want \"low\"", got)
	}
	var thinking map[string]string
	if err := json.Unmarshal(together["thinking"], &thinking); err != nil || thinking["type"] != "disabled" {
		t.Errorf("object-valued default not preserved: %s (err=%v)", together["thinking"], err)
	}

	if d := byProvider["baseten"].RequestDefaults; d != nil {
		t.Errorf("an entry without request_defaults must carry none, got %v", d)
	}
}
