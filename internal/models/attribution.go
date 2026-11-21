package models

import "math"

// InputSlice intersects a result's estimated position in the prompt with the
// reported cached prefix. Anthropic orders hour writes before five-minute writes.
// Input after the last cache breakpoint is ordinary input.
func InputSlice(model string, prompt, cached, write1h, write5m, start, size int) ModelInput {
	start = min(max(0, start), prompt)
	end := start + min(size, prompt-start)
	overlap := func(lo, hi int) int { return max(0, min(end, hi)-max(start, lo)) }
	return ModelInput{Model: model, Prompt: prompt, Input: end - start,
		Cached: overlap(0, cached), Write1h: overlap(cached, cached+write1h),
		Write5m: overlap(cached+write1h, cached+write1h+write5m)}
}

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
