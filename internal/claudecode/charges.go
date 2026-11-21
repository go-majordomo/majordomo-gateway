package claudecode

import "github.com/go-majordomo/majordomo-gateway/internal/models"

// Placement is a tool result's estimated position in its first carrying request.
type Placement struct{ Start, Tokens int }
type carriedResult struct {
	ToolResult
	Placement
	charges []models.ModelInput
}

// carriedResults allocates each result once, charging the initial request and
// subsequent requests until compaction. Newly appended results take precedence
// where estimated positions overlap.
func (th *Thread) carriedResults() []carriedResult {
	placements := th.placements()
	byGap := make(map[int][]int)
	var out []carriedResult
	for i, res := range th.Results {
		pos := placements[i]
		if pos.Tokens < 0 {
			continue
		}
		byGap[res.Gap] = append(byGap[res.Gap], len(out))
		out = append(out, carriedResult{ToolResult: res, Placement: pos})
	}
	var active []int
	ends := make([]int, len(out))
	for j, r := range th.Requests {
		if j > 0 && r.Context() < th.Requests[j-1].Context() {
			active = nil
		}
		appended := byGap[j]
		for _, idx := range appended {
			pos := out[idx].Placement
			for _, prior := range active {
				ends[prior] = min(ends[prior], pos.Start)
			}
			ends[idx] = pos.Start + pos.Tokens
		}
		active = append(active, appended...)
		carried := active[:0]
		for _, idx := range active {
			result := &out[idx]
			if j > result.Gap && ends[idx] <= result.Start {
				continue
			}
			size := max(0, ends[idx]-result.Start)
			c := models.InputSlice(r.Model, r.Context(), r.CacheReadTokens, r.CacheCreation1hTokens, r.CacheCreation5mTokens, result.Start, size)
			result.charges = append(result.charges, c)
			carried = append(carried, idx)
		}
		active = carried
	}
	return out
}

func (th *Thread) placements() []Placement {
	sizes := th.ResultTokens()
	starts := th.resultStarts(sizes)
	positions := make([]Placement, len(starts))
	for i, start := range starts {
		size, ok := sizes[i]
		if !ok {
			size = -1
		}
		positions[i] = Placement{Start: start, Tokens: size}
	}
	return positions
}
