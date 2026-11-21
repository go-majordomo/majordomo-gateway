package claudecode

import (
	"math"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// Calculator prices token usage. Satisfied by *pricing.Service.
type Calculator interface {
	Calculate(metrics *models.UsageMetrics) models.Cost
}

// ToolCost is the cost of carrying one tool call's result within a range of
// requests: written into the context once, then re-read from the prompt cache.
type ToolCost struct {
	ToolName  string
	Tokens    int
	Earlier   bool // the call was made before this range; only its re-reads fall here
	Rereads   int
	WriteCost float64
	ReadCost  float64
}

// Cost is the total cost attributed to the tool result in this range.
func (t ToolCost) Cost() float64 { return t.WriteCost + t.ReadCost }

// Breakdown splits the cost of a range of requests into what it was spent on.
// The parts sum to Total: Output + Tools + Definitions + Rest.
type Breakdown struct {
	Requests    int
	Total       float64
	Output      float64            // replies, reasoning and tool-call arguments
	Tools       []ToolCost         // carrying tool results
	Definitions map[string]float64 // MCP server -> tool-definition cost ("" = shared preamble)
	Rest        float64            // system prompt and conversation history
	Unpriced    map[string]bool    // models with no known price (counted as $0)
}

// ToolsTotal is the summed cost of all tool results.
func (b Breakdown) ToolsTotal() float64 {
	sum := 0.0
	for _, t := range b.Tools {
		sum += t.Cost()
	}
	return sum
}

// DefinitionsTotal is the summed cost of MCP tool definitions.
func (b Breakdown) DefinitionsTotal() float64 {
	sum := 0.0
	for _, v := range b.Definitions {
		sum += v
	}
	return sum
}

// MCPDefinitions is the measured token size of MCP tool definitions: per server
// (when connected alongside the others) plus a shared preamble sent once.
type MCPDefinitions struct {
	PerServer map[string]int
	Shared    int
}

// Total is the definitions' total token size.
func (d *MCPDefinitions) Total() int {
	if d == nil {
		return 0
	}
	t := d.Shared
	for _, v := range d.PerServer {
		t += v
	}
	return t
}

const anthropic = "anthropic"

// Explain prices requests [from, to) of a thread and splits the cost into model
// output, each tool result carried, MCP tool definitions (when defs is given)
// and the rest of the context.
func Explain(calc Calculator, th *Thread, from, to int, defs *MCPDefinitions) Breakdown {
	return explain(calc, th, th.carriedResults(), from, to, defs)
}

func explain(calc Calculator, th *Thread, results []carriedResult, from, to int, defs *MCPDefinitions) Breakdown {
	b := Breakdown{Requests: to - from, Definitions: map[string]float64{}, Unpriced: map[string]bool{}}
	byCall := map[int]*ToolCost{}
	var order []int

	for j := from; j < to; j++ {
		r := th.Requests[j]
		provider := r.Provider
		if provider == "" {
			provider = anthropic
		}
		total := calc.Calculate(&models.UsageMetrics{
			Provider:              provider,
			Model:                 r.Model,
			InputTokens:           r.Context(),
			OutputTokens:          r.OutputTokens,
			CachedTokens:          r.CacheReadTokens,
			CacheCreationTokens:   r.CacheCreationTokens(),
			CacheCreation5mTokens: r.CacheCreation5mTokens,
			CacheCreation1hTokens: r.CacheCreation1hTokens,
		})
		if !total.ModelAliasFound {
			b.Unpriced[r.Model] = true
		}
		parts := total.OutputCost
		defLimit := r.Context()
		for i, res := range results {
			k := j - res.Gap
			if k < 0 || k >= len(res.charges) {
				continue
			}
			tc := byCall[i]
			if tc == nil {
				tc = &ToolCost{ToolName: res.ToolName, Tokens: res.Tokens, Earlier: res.Gap < from}
				byCall[i] = tc
				order = append(order, i)
			}
			slice := res.charges[k]
			if slice.Input > 0 {
				defLimit = min(defLimit, res.Start)
			}
			c := calc.Calculate(slice.Metrics(provider)).TotalCost
			if j == res.Gap {
				tc.WriteCost += c
			} else {
				tc.ReadCost += c
				if slice.Input > 0 {
					tc.Rereads++
				}
			}
			parts += c
		}

		// Definitions sit at the start of the context, so this request's cache
		// reads cover them first (possibly left by an earlier session with the
		// same prefix); any remainder was written.
		requestDefs := defs
		if r.Definitions != nil {
			requestDefs = &MCPDefinitions{PerServer: r.Definitions}
		}
		if weight := requestDefs.Total(); weight > 0 {
			slice := models.InputSlice(r.Model, r.Context(), r.CacheReadTokens, r.CacheCreation1hTokens, r.CacheCreation5mTokens, 0, min(weight, defLimit))
			cost := calc.Calculate(slice.Metrics(provider)).TotalCost
			for name, size := range requestDefs.PerServer {
				b.Definitions[name] += cost * float64(size) / float64(weight)
			}
			if requestDefs.Shared > 0 {
				b.Definitions[""] += cost * float64(requestDefs.Shared) / float64(weight)
			}
			parts += cost
		}
		b.Total += total.TotalCost
		b.Output += total.OutputCost
		b.Rest += math.Max(total.TotalCost-parts, 0)
	}
	for _, i := range order {
		b.Tools = append(b.Tools, *byCall[i])
	}
	return b
}

// Attributed is the sum of all parts; it equals Total up to rounding when no
// part is overestimated.
func (b Breakdown) Attributed() float64 {
	return b.Output + b.ToolsTotal() + b.DefinitionsTotal() + b.Rest
}
