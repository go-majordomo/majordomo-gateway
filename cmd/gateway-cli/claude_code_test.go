package main

import (
	"math"
	"testing"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// tierCalc prices $1/M input, $0.10/M cached, 5x for a prompt above 100K tokens.
type tierCalc struct{}

func (tierCalc) Calculate(m *models.UsageMetrics) models.Cost {
	rate := 1.0
	if max(m.InputTokens, m.PromptTokens) > 100_000 {
		rate = 5
	}
	in := float64(m.InputTokens-m.CachedTokens) * rate / 1e6
	cached := float64(m.CachedTokens) * 0.1 * rate / 1e6
	return models.Cost{InputCost: in, CachedCost: cached, TotalCost: in + cached, ModelAliasFound: true}
}

// The projection prices one request at that request's prompt size, then
// multiplies: 1,000 small requests must not be priced as one huge prompt.
func TestProjectOverheadPricesPerRequest(t *testing.T) {
	cached, plain, ok := claudecode.ProjectOverhead(tierCalc{}, "anthropic", "m", 6500, 7000, 1000)
	if !ok {
		t.Fatal("unpriced")
	}
	if want := 6500 * 0.1 / 1e6 * 1000; math.Abs(cached-want) > 1e-12 {
		t.Errorf("cached = %v, want %v", cached, want)
	}
	if want := 6500 / 1e6 * 1000.0; math.Abs(plain-want) > 1e-12 {
		t.Errorf("plain = %v, want %v", plain, want)
	}

	// A measured request above the threshold is priced at the higher rate.
	_, plain, _ = claudecode.ProjectOverhead(tierCalc{}, "anthropic", "m", 6500, 150_000, 1000)
	if want := 6500 * 5 / 1e6 * 1000.0; math.Abs(plain-want) > 1e-12 {
		t.Errorf("long prompt plain = %v, want %v", plain, want)
	}
}
