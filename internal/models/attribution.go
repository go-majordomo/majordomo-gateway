package models

import "math"

func (c ModelInput) Metrics(provider string) *UsageMetrics {
	return &UsageMetrics{Provider: provider, BaseTier: c.Prompt == 0, Model: c.Model, PromptTokens: c.Prompt,
		InputTokens: c.Input, CachedTokens: c.Cached, CacheCreationTokens: c.Write1h + c.Write5m,
		CacheCreation5mTokens: c.Write5m, CacheCreation1hTokens: c.Write1h}
}

// Valid reports whether the slice fits its carrying prompt and cache classes.
func (c ModelInput) Valid() bool {
	return c.Model != "" && len(c.Model) <= 100 && c.Prompt >= 0 && c.Prompt <= math.MaxInt32 &&
		c.Input >= 0 && c.Input <= math.MaxInt32 && (c.Prompt == 0 || c.Input <= c.Prompt) && c.Cached >= 0 && c.Cached <= c.Input &&
		c.Write1h >= 0 && c.Write1h <= c.Input-c.Cached && c.Write5m >= 0 && c.Write5m <= c.Input-c.Cached-c.Write1h
}
