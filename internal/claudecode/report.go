package claudecode

import (
	"math"
	"sort"
	"strings"
	"time"
)

// ToolLine is the cost of carrying one tool's results within a turn, for all
// calls of that tool (or, with per-call detail, one call).
type ToolLine struct {
	Tool      string  `json:"tool"`
	MCPServer string  `json:"mcp_server,omitempty"`
	Calls     int     `json:"calls"`
	Tokens    int     `json:"result_tokens"`
	Rereads   int     `json:"rereads"`
	Earlier   bool    `json:"earlier,omitempty"` // results of calls made in an earlier turn, re-read here
	Cost      float64 `json:"cost"`
}

// PartReport is the cost of one thread's requests within a turn, split into
// model output, tool results, MCP tool definitions and the rest. The parts sum
// to Total.
type PartReport struct {
	Requests         int                `json:"requests"`
	Total            float64            `json:"total"`
	Output           float64            `json:"output"`
	ToolsTotal       float64            `json:"tool_results"`
	Tools            []ToolLine         `json:"tools,omitempty"`
	DefinitionsTotal float64            `json:"mcp_definitions,omitempty"`
	Definitions      map[string]float64 `json:"mcp_definitions_by_server,omitempty"` // "(shared preamble)" for the shared part
	Rest             float64            `json:"system_prompt_and_history"`
}

// SubagentReport is one subagent launched during a turn.
type SubagentReport struct {
	Name string `json:"name"`
	Type string `json:"type"`
	PartReport
}

// TurnReport is the cost of answering one message the user typed.
type TurnReport struct {
	Number    int              `json:"turn"`
	At        time.Time        `json:"at"`
	Prompt    string           `json:"prompt"` // first words of the message (local only)
	Total     float64          `json:"total"`
	Main      PartReport       `json:"main"`
	Subagents []SubagentReport `json:"subagents,omitempty"`
	Warning   string           `json:"warning,omitempty"`
}

// Report explains where a session's API-equivalent cost went, per turn.
type Report struct {
	SessionID string       `json:"session_id"`
	Turns     []TurnReport `json:"turns"`
	Total     float64      `json:"total"`
	Unpriced  []string     `json:"unpriced_models,omitempty"`
	Note      string       `json:"note"`
}

// SharedPreambleName labels the shared MCP preamble in definition breakdowns.
const SharedPreambleName = "(shared preamble)"

const reportNote = "Costs are API-equivalent: what these tokens cost at API list prices. " +
	"Tool and definition costs are estimated shares of the turn total. Result positions are reconstructed from the transcript and matched to each request’s reported cache reads and writes."

// ReportOptions tune BuildReport.
type ReportOptions struct {
	Definitions *MCPDefinitions // measured MCP tool definitions; nil folds them into Rest
	PerCall     bool            // one ToolLine per call instead of per tool
}

// BuildReport prices a session and explains where each turn's cost went.
func BuildReport(calc Calculator, s *Session, opts ReportOptions) Report {
	r := Report{SessionID: s.ID, Note: reportNote}
	unpriced := map[string]bool{}
	mainResults := s.Main.carriedResults()
	for _, t := range s.Turns() {
		b := explain(calc, s.Main, mainResults, t.From, t.To, opts.Definitions)
		tr := TurnReport{Number: t.Number, At: t.At, Prompt: snippet(t.Prompt, 80), Main: partReport(b, opts.PerCall)}
		tr.Total = b.Total
		if diff := b.Attributed() - b.Total; b.Total > 0 && diff/b.Total > 0.01 {
			tr.Warning = "parts exceed the total; a tool result or definition size is overestimated"
		}
		for m := range b.Unpriced {
			unpriced[m] = true
		}
		for _, sub := range t.Subagents {
			sb := Explain(calc, sub, 0, len(sub.Requests), nil)
			for m := range sb.Unpriced {
				unpriced[m] = true
			}
			tr.Subagents = append(tr.Subagents, SubagentReport{Name: AgentStepName(sub), Type: sub.AgentType, PartReport: partReport(sb, opts.PerCall)})
			tr.Total += sb.Total
		}
		r.Turns = append(r.Turns, tr)
		r.Total += tr.Total
	}
	for m := range unpriced {
		r.Unpriced = append(r.Unpriced, m)
	}
	sort.Strings(r.Unpriced)
	return r
}

func partReport(b Breakdown, perCall bool) PartReport {
	p := PartReport{
		Requests:         b.Requests,
		Total:            b.Total,
		Output:           b.Output,
		ToolsTotal:       b.ToolsTotal(),
		DefinitionsTotal: b.DefinitionsTotal(),
		Rest:             b.Rest,
	}
	if len(b.Definitions) > 0 {
		p.Definitions = map[string]float64{}
		for name, c := range b.Definitions {
			if name == "" {
				name = SharedPreambleName
			}
			p.Definitions[name] = c
		}
	}

	type key struct {
		tool    string
		earlier bool
	}
	groups := map[key]*ToolLine{}
	for _, t := range b.Tools {
		if perCall {
			p.Tools = append(p.Tools, ToolLine{Tool: t.ToolName, MCPServer: MCPServer(t.ToolName), Calls: 1,
				Tokens: t.Tokens, Rereads: t.Rereads, Earlier: t.Earlier, Cost: t.Cost()})
			continue
		}
		k := key{t.ToolName, t.Earlier}
		g := groups[k]
		if g == nil {
			g = &ToolLine{Tool: t.ToolName, MCPServer: MCPServer(t.ToolName), Earlier: t.Earlier}
			groups[k] = g
		}
		g.Calls++
		g.Tokens += t.Tokens
		g.Rereads += t.Rereads
		g.Cost += t.Cost()
	}
	for _, g := range groups {
		p.Tools = append(p.Tools, *g)
	}
	sort.SliceStable(p.Tools, func(i, j int) bool { return p.Tools[i].Cost > p.Tools[j].Cost })
	return p
}

// MCPServer returns the MCP server of a tool named "mcp__<server>__<tool>", or "".
func MCPServer(tool string) string {
	parts := strings.SplitN(tool, "__", 3)
	if len(parts) == 3 && parts[0] == "mcp" {
		return parts[1]
	}
	return ""
}

// Components flattens a report into non-overlapping cost components that sum to
// its total, keyed so two sessions can be compared: "model output",
// "tool: <name>" (main agent's tool results), "mcp definitions: <server>",
// "subagent: <type>" (all of a subagent type's cost) and "system prompt & history".
func (r Report) Components() map[string]float64 {
	c := map[string]float64{}
	for _, t := range r.Turns {
		c["model output"] += t.Main.Output
		c["system prompt & history"] += t.Main.Rest
		for _, tl := range t.Main.Tools {
			c["tool: "+tl.Tool] += tl.Cost
		}
		for name, v := range t.Main.Definitions {
			c["mcp definitions: "+name] += v
		}
		for _, s := range t.Subagents {
			c["subagent: "+s.Type] += s.Total
		}
	}
	return c
}

// Requests is the number of model requests in the session, subagents included.
func (r Report) Requests() int {
	n := 0
	for _, t := range r.Turns {
		n += t.Main.Requests
		for _, s := range t.Subagents {
			n += s.Requests
		}
	}
	return n
}

// ComparisonLine is one cost component before and after a change.
type ComparisonLine struct {
	Component string  `json:"component"`
	Before    float64 `json:"before"`
	After     float64 `json:"after"`
	Delta     float64 `json:"delta"`
}

// Comparison sets two sessions side by side, component by component, sorted
// by the size of the change.
type Comparison struct {
	Before         string           `json:"before_session"`
	After          string           `json:"after_session"`
	BeforeTotal    float64          `json:"before_total"`
	AfterTotal     float64          `json:"after_total"`
	Delta          float64          `json:"delta"`
	DeltaPercent   float64          `json:"delta_percent"`
	BeforeRequests int              `json:"before_requests"`
	AfterRequests  int              `json:"after_requests"`
	Lines          []ComparisonLine `json:"components"`
	Note           string           `json:"note"`
}

// Compare explains how the cost changed from one session to another.
func Compare(before, after Report) Comparison {
	cmp := Comparison{
		Before: before.SessionID, After: after.SessionID,
		BeforeTotal: before.Total, AfterTotal: after.Total, Delta: after.Total - before.Total,
		BeforeRequests: before.Requests(), AfterRequests: after.Requests(),
		Note: "API-equivalent costs. Components are non-overlapping and sum to each total; " +
			"a subagent's line includes its own tools and context.",
	}
	if before.Total > 0 {
		cmp.DeltaPercent = 100 * cmp.Delta / before.Total
	}
	b, a := before.Components(), after.Components()
	keys := map[string]bool{}
	for k := range b {
		keys[k] = true
	}
	for k := range a {
		keys[k] = true
	}
	for k := range keys {
		if b[k] == 0 && a[k] == 0 {
			continue
		}
		cmp.Lines = append(cmp.Lines, ComparisonLine{Component: k, Before: b[k], After: a[k], Delta: a[k] - b[k]})
	}
	sort.Slice(cmp.Lines, func(i, j int) bool {
		di, dj := math.Abs(cmp.Lines[i].Delta), math.Abs(cmp.Lines[j].Delta)
		if di != dj {
			return di > dj
		}
		return cmp.Lines[i].Component < cmp.Lines[j].Component
	})
	return cmp
}

func snippet(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if chars := []rune(s); len(chars) > n {
		return string(chars[:n-1]) + "…"
	}
	return s
}

// SessionReport optionally measures definitions before pricing a local session.
func SessionReport(calc Calculator, s *Session, mcpConfig string, perCall bool) (Report, error) {
	opts := ReportOptions{PerCall: perCall}
	if mcpConfig != "" && len(s.Main.Requests) > 0 {
		m, err := MeasureMCP(s.Main.Requests[0].Model, mcpConfig)
		if err != nil {
			return Report{}, err
		}
		opts.Definitions = m.Definitions()
	}
	return BuildReport(calc, s, opts), nil
}
