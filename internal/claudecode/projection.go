package claudecode

import "github.com/go-majordomo/majordomo-gateway/internal/models"

// ProjectOverhead prices tokens carried by every one of requests requests,
// each with a prompt of promptTokens, either read from the prompt cache or
// sent uncached. One request is priced, then multiplied: pricing the total as
// a single prompt would wrongly cross a long-context threshold.
func ProjectOverhead(calc Calculator, provider, model string, tokens, promptTokens, requests int) (cached, plain float64, ok bool) {
	c := calc.Calculate(&models.UsageMetrics{Provider: provider, BaseTier: promptTokens == 0, Model: model, InputTokens: tokens, CachedTokens: tokens, PromptTokens: promptTokens})
	p := calc.Calculate(&models.UsageMetrics{Provider: provider, BaseTier: promptTokens == 0, Model: model, InputTokens: tokens, PromptTokens: promptTokens})
	return c.TotalCost * float64(requests), p.TotalCost * float64(requests), p.ModelAliasFound
}
