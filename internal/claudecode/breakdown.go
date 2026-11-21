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
	b := Breakdown{Requests: to - from, Definitions: map[string]float64{}, Unpriced: map[string]bool{}}
	sizes := th.ResultTokens()
	byCall := map[int]*ToolCost{}
	var order []int

	for j := from; j < to; j++ {
		r := th.Requests[j]
		total := calc.Calculate(&models.UsageMetrics{
			Provider:              anthropic,
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
		// written prices n tokens entering this request uncached, at its mix of
		// 5-minute and 1-hour cache writes (or plain input if it wrote none).
		written := func(n int) float64 { return writeCost(calc, r, n) }
		read := func(n int) float64 { return readCost(calc, r, n) }

		parts := total.OutputCost
		for i, res := range th.Results {
			tokens, sent := sizes[i]
			if !sent || res.Gap > j || j > res.Gap+th.RereadsAfter(res.Gap) {
				continue
			}
			tc := byCall[i]
			if tc == nil {
				tc = &ToolCost{ToolName: res.ToolName, Tokens: tokens, Earlier: res.Gap < from}
				byCall[i] = tc
				order = append(order, i)
			}
			var c float64
			if j == res.Gap {
				c = written(tokens)
				tc.WriteCost += c
			} else {
				c = read(tokens)
				tc.ReadCost += c
				tc.Rereads++
			}
			parts += c
		}

		// Definitions sit at the start of the context, so this request's cache
		// reads cover them first (possibly left by an earlier session with the
		// same prefix); any remainder was written.
		if defs != nil && defs.Total() > 0 {
			defTokens := defs.Total()
			cached := min(r.CacheReadTokens, defTokens)
			share := func(n int) float64 {
				c := float64(cached) / float64(defTokens)
				return c*read(n) + (1-c)*written(n)
			}
			for name, n := range defs.PerServer {
				c := share(n)
				b.Definitions[name] += c
				parts += c
			}
			if defs.Shared > 0 {
				c := share(defs.Shared)
				b.Definitions[""] += c
				parts += c
			}
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

// writeCost prices n tokens entering request r uncached, at r's price tier.
func writeCost(calc Calculator, r Request, n int) float64 {
	return calc.Calculate(r.writtenInput(n).Metrics(anthropic)).TotalCost
}

func (r Request) writtenInput(n int) models.ModelInput {
	c := models.ModelInput{Model: r.Model, Prompt: r.Context(), Input: n}
	if w := r.CacheCreationTokens(); w > 0 {
		c.Write1h = int(math.Round(float64(n) * float64(r.CacheCreation1hTokens) / float64(w)))
		c.Write5m = n - c.Write1h
	}
	return c
}

func (r Request) readInput(n int) models.ModelInput {
	return models.ModelInput{Model: r.Model, Prompt: r.Context(), Input: n, Cached: n}
}

func readCost(calc Calculator, r Request, n int) float64 {
	return calc.Calculate(r.readInput(n).Metrics(anthropic)).TotalCost
}

// Attributed is the sum of all parts; it equals Total up to rounding when no
// part is overestimated.
func (b Breakdown) Attributed() float64 {
	return b.Output + b.ToolsTotal() + b.DefinitionsTotal() + b.Rest
}
