package models_test

import (
	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
	"math"
	"testing"
	"time"
)

func TestSavingsFromShorteningFreshResult(t *testing.T) {
	calc := pricing.NewService("", "../../model_catalog.json", "../../model_aliases.json", time.Hour)
	defer calc.Close()
	const model = "gpt-6.2-sol"
	costs := func(size int) (float64, float64) {
		const prefix = 9000
		prompt := prefix + size
		total := calc.Calculate(&models.UsageMetrics{Provider: "openai", Model: model, InputTokens: prompt, CachedTokens: prefix, OutputTokens: 20})
		tool := models.InputSlice(model, prompt, prefix, 0, 0, prefix, size)
		return total.TotalCost, calc.Calculate(tool.Metrics("openai")).TotalCost
	}
	before, toolBefore := costs(1000)
	after, toolAfter := costs(500)
	for _, saved := range []float64{before - after, toolBefore - toolAfter} {
		if math.Abs(1000*saved-1) > 1e-9 {
			t.Fatalf("saved per 1000 repetitions = %g, want $1", 1000*saved)
		}
	}
}
func TestInputSliceCrossesCacheBoundaries(t *testing.T) {
	c := models.InputSlice("model", 1000, 500, 200, 100, 450, 400)
	if c.Input != 400 || c.Cached != 50 || c.Write1h != 200 || c.Write5m != 100 {
		t.Fatal(c)
	}
}
