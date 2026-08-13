package pricing

import (
	"math"
	"os"
	"strings"
	"testing"

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
	tg, ok := s.resolve("together", "deepseek-ai/DeepSeek-V4-Pro")
	if !ok {
		t.Fatal("together price for shared id missing")
	}
	bt, ok := s.resolve("baseten", "deepseek-ai/DeepSeek-V4-Pro")
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
