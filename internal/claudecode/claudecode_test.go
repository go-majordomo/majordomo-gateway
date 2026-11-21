package claudecode

import (
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// Fixture builders for transcript lines.

func usage(input, read, write, write1h, out int) string {
	return `"usage":{"input_tokens":` + itoa(input) + `,"output_tokens":` + itoa(out) +
		`,"cache_read_input_tokens":` + itoa(read) + `,"cache_creation_input_tokens":` + itoa(write) +
		`,"cache_creation":{"ephemeral_1h_input_tokens":` + itoa(write1h) + `}}`
}

// assistant is one request; each tool id becomes a tool_use block. Tool ids
// starting with "agent" are Agent (subagent) calls, others an MCP tool.
func assistant(req string, read, write, out int, toolIDs ...string) string {
	blocks := `{"type":"text","text":"working"}`
	for _, id := range toolIDs {
		name := "mcp__crm__get_leads"
		if strings.HasPrefix(id, "agent") {
			name = "Agent"
		}
		blocks += `,{"type":"tool_use","id":"` + id + `","name":"` + name + `","input":{"secret":"arg"}}`
	}
	return `{"type":"assistant","sessionId":"S","cwd":"/proj","requestId":"` + req +
		`","timestamp":"2026-11-17T10:00:00Z","message":{"model":"claude-haiku-5-5","content":[` + blocks + `],` +
		usage(0, read, write, write, out) + `}}`
}

func prompt(text string) string {
	return `{"type":"user","cwd":"/proj","timestamp":"2026-11-17T10:00:00Z","message":{"role":"user","content":"` + text + `"}}`
}

func result(id string, chars int) string {
	return `{"type":"user","cwd":"/proj","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"` + id +
		`","content":[{"type":"text","text":"` + strings.Repeat("a", chars) + `"}]}]}}`
}

func itoa(i int) string { return strconv.Itoa(i) }

func writeFile(t *testing.T, path string, lines ...string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// writeSession writes a two-turn session:
//
//	turn 1: calls an MCP tool and launches a subagent; a background-task
//	        notification arrives (part of turn 1, not a new turn)
//	turn 2: no tools; re-reads turn 1's results
func writeSession(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	main := filepath.Join(root, "proj", "S.jsonl")
	writeFile(t, main,
		prompt("find leads"),
		assistant("r1", 0, 1000, 100, "t1", "agent1"),
		assistant("r1", 0, 1000, 120, "t1", "agent1"), // same request, later content block
		result("t1", 400),
		result("agent1", 100),
		assistant("r2", 1000, 600, 50), // growth 1600-1000-120 = 480 -> t1 384, agent1 96
		prompt("<task-notification> done </task-notification>"),
		assistant("r3", 1600, 60, 20),
		prompt("summarize"),
		assistant("r4", 1660, 30, 10),
	)
	writeFile(t, filepath.Join(root, "proj", "S", "subagents", "agent-x1234567890.jsonl"),
		`{"type":"assistant","sessionId":"S","cwd":"/proj","requestId":"s1","timestamp":"2026-11-17T10:00:00Z",`+
			`"message":{"model":"claude-haiku-5-5","content":[],`+usage(2, 0, 300, 300, 30)+`}}`,
	)
	writeFile(t, filepath.Join(root, "proj", "S", "subagents", "agent-x1234567890.meta.json"),
		`{"agentType":"researcher","toolUseId":"agent1"}`)
	return main
}

func TestParseThread(t *testing.T) {
	th, err := parseFixture(writeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(th.Requests) != 4 {
		t.Fatalf("requests = %d, want 4 (content-block lines merged)", len(th.Requests))
	}
	if r := th.Requests[0]; r.OutputTokens != 120 || r.CacheCreation1hTokens != 1000 || r.CacheCreation5mTokens != 0 {
		t.Errorf("request 1 = %+v, want the largest output and a 1h cache write", r)
	}
	if len(th.Prompts) != 2 {
		t.Errorf("prompts = %d, want 2 (task notification is not typed)", len(th.Prompts))
	}
	if th.ToolCaller["agent1"] != 0 || th.Results[0].ToolName != "mcp__crm__get_leads" {
		t.Errorf("tool attribution wrong: caller=%v results=%+v", th.ToolCaller, th.Results)
	}

	sizes := th.ResultTokens()
	if sizes[0] != 384 || sizes[1] != 96 {
		t.Errorf("result sizes = %v, want growth 480 split 384/96 by length", sizes)
	}
	if n := th.RereadsAfter(1); n != 2 {
		t.Errorf("rereads after request 2 = %d, want 2", n)
	}
}

func TestRereadsStopAtCompaction(t *testing.T) {
	path := filepath.Join(t.TempDir(), "C.jsonl")
	writeFile(t, path,
		prompt("go"),
		assistant("a", 0, 1000, 10, "t1"),
		result("t1", 100),
		assistant("b", 1000, 200, 10),
		assistant("c", 1200, 10, 10),
		assistant("d", 50, 100, 10), // context shrank: compacted
		assistant("e", 150, 10, 10),
	)
	th, err := parseFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	if n := th.RereadsAfter(1); n != 1 {
		t.Errorf("rereads = %d, want 1 (stop at compaction)", n)
	}
}

func TestSessionTurnsAndImport(t *testing.T) {
	s, err := loadFixture(writeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	turns := s.Turns()
	if len(turns) != 2 || turns[0].From != 0 || turns[0].To != 3 || turns[1].From != 3 || turns[1].To != 4 {
		t.Fatalf("turns = %+v, want [0,3) and [3,4)", turns)
	}
	if len(turns[0].Subagents) != 1 || len(turns[1].Subagents) != 0 {
		t.Fatalf("the subagent belongs to turn 1")
	}

	imp := BuildImport(s, "", map[string]string{"team": "acme"})
	if imp.SessionID != "S" || len(imp.Requests) != 5 {
		t.Fatalf("import = %d requests for %q, want 5 for S", len(imp.Requests), imp.SessionID)
	}
	paths := map[string]int{}
	for _, r := range imp.Requests {
		paths[r.SpanPath]++
	}
	want := map[string]int{"claude-code/turn-1": 3, "claude-code/turn-2": 1, "claude-code/turn-1/agent:researcher-x1234567": 1}
	for p, n := range want {
		if paths[p] != n {
			t.Errorf("span %q has %d requests, want %d (all: %v)", p, paths[p], n, paths)
		}
	}
	if len(imp.ToolSpans) != 2 {
		t.Fatalf("tool spans = %d, want 2", len(imp.ToolSpans))
	}
	ts := imp.ToolSpans[0]
	if ts.ToolName != "mcp__crm__get_leads" || ts.SpanPath != "claude-code/turn-1" ||
		ts.ResultTokens != 384 || ts.Charges[0].Write1h != 384 || len(ts.Charges) != 3 {
		t.Errorf("tool span = %+v", ts)
	}
}

// fakeCalc prices claude-haiku-5-5 at $1/M input, $0.10/M cached, $5/M output,
// with cache writes at 1.25x (5m) / 2x (1h) input, mirroring pricing.Service.
type fakeCalc struct{}

func (fakeCalc) Calculate(m *models.UsageMetrics) models.Cost {
	if m.Model != "claude-haiku-5-5" {
		return models.Cost{}
	}
	fresh := float64(m.InputTokens-m.CachedTokens-m.CacheCreationTokens) * 1 / 1e6
	cached := float64(m.CachedTokens) * 0.1 / 1e6
	create := (float64(m.CacheCreation5mTokens)*1.25 + float64(m.CacheCreation1hTokens)*2) / 1e6
	out := float64(m.OutputTokens) * 5 / 1e6
	return models.Cost{InputCost: fresh, CachedCost: cached, CacheCreationCost: create, OutputCost: out,
		TotalCost: fresh + cached + create + out, ModelAliasFound: true}
}

func TestExplainPartsSumToTotal(t *testing.T) {
	s, err := loadFixture(writeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	defs := &MCPDefinitions{PerServer: map[string]int{"crm": 200}, Shared: 50}
	for _, turn := range s.Turns() {
		for _, d := range []*MCPDefinitions{nil, defs} {
			b := Explain(fakeCalc{}, s.Main, turn.From, turn.To, d)
			if math.Abs(b.Attributed()-b.Total) > 1e-12 {
				t.Errorf("turn %d (defs=%v): parts %.9f != total %.9f", turn.Number, d != nil, b.Attributed(), b.Total)
			}
		}
	}

	// Turn 2 only re-reads turn 1's results: t1 is 384 tokens at $0.10/M cached.
	b := Explain(fakeCalc{}, s.Main, 3, 4, nil)
	var t1 *ToolCost
	for i := range b.Tools {
		if b.Tools[i].ToolName == "mcp__crm__get_leads" {
			t1 = &b.Tools[i]
		}
	}
	if t1 == nil || !t1.Earlier || t1.Rereads != 1 || math.Abs(t1.ReadCost-384*0.1/1e6) > 1e-15 || t1.WriteCost != 0 {
		t.Errorf("turn 2 re-read of t1 = %+v", t1)
	}

	// Turn 1 writes t1 at the carrying request's all-1h rate (2x input).
	b = Explain(fakeCalc{}, s.Main, 0, 3, nil)
	for _, tc := range b.Tools {
		if tc.ToolName == "mcp__crm__get_leads" && math.Abs(tc.WriteCost-384*2/1e6) > 1e-15 {
			t.Errorf("t1 write cost = %v, want %v", tc.WriteCost, 384*2/1e6)
		}
	}
}

func TestIsInjected(t *testing.T) {
	for text, want := range map[string]bool{
		"<task-notification>x</task-notification>": true,
		"  <system-reminder>x":                     true,
		"Please summarize":                         false,
		"<command-name>/review</command-name>":     false,
	} {
		if isInjected(text) != want {
			t.Errorf("isInjected(%q) != %v", text, want)
		}
	}
}

func TestProjectMatcher(t *testing.T) {
	match, err := ProjectMatcher("/work/app")
	if err != nil {
		t.Fatal(err)
	}
	for cwd, want := range map[string]bool{"/work/app": true, "/work/app/sub": true, "/work/application": false, "": false} {
		if match(cwd) != want {
			t.Errorf("match(%q) != %v", cwd, want)
		}
	}
}

func TestReportComponentsSumToTotal(t *testing.T) {
	s, err := loadFixture(writeSession(t))
	if err != nil {
		t.Fatal(err)
	}
	r := BuildReport(fakeCalc{}, s, ReportOptions{Definitions: &MCPDefinitions{PerServer: map[string]int{"crm": 200}, Shared: 50}})
	if len(r.Turns) != 2 || r.Requests() != 5 {
		t.Fatalf("report has %d turns / %d requests, want 2 / 5", len(r.Turns), r.Requests())
	}
	sum := 0.0
	for _, v := range r.Components() {
		sum += v
	}
	if math.Abs(sum-r.Total) > 1e-12 {
		t.Errorf("components sum %.9f != total %.9f", sum, r.Total)
	}
	if r.Components()["subagent: researcher"] == 0 || r.Components()["mcp definitions: crm"] == 0 {
		t.Errorf("missing components: %v", r.Components())
	}
}

// A feature that adds a tool call shows up as that tool's component in the diff.
func TestCompare(t *testing.T) {
	before := filepath.Join(t.TempDir(), "B.jsonl")
	writeFile(t, before,
		prompt("go"),
		assistant("a", 0, 1000, 10),
	)
	after := filepath.Join(t.TempDir(), "A.jsonl")
	writeFile(t, after,
		prompt("go"),
		assistant("a", 0, 1000, 10, "t1"),
		result("t1", 400),
		assistant("b", 1000, 410, 10),
	)
	sb, _ := loadFixture(before)
	sa, _ := loadFixture(after)
	cmp := Compare(BuildReport(fakeCalc{}, sb, ReportOptions{}), BuildReport(fakeCalc{}, sa, ReportOptions{}))

	if cmp.Delta <= 0 || cmp.BeforeRequests != 1 || cmp.AfterRequests != 2 {
		t.Fatalf("comparison = %+v", cmp)
	}
	sum := 0.0
	var tool *ComparisonLine
	for i, l := range cmp.Lines {
		sum += l.Delta
		if l.Component == "tool: mcp__crm__get_leads" {
			tool = &cmp.Lines[i]
		}
	}
	if math.Abs(sum-cmp.Delta) > 1e-12 {
		t.Errorf("line deltas sum %.9f != total delta %.9f", sum, cmp.Delta)
	}
	if tool == nil || tool.Before != 0 || tool.After <= 0 {
		t.Errorf("the added tool should appear as a new component: %+v", cmp.Lines)
	}
}

// tierCalc is fakeCalc with every rate 5x for a prompt above 100K tokens,
// like Claude Haiku 5.5's long-context tier.
type tierCalc struct{}

func (tierCalc) Calculate(m *models.UsageMetrics) models.Cost {
	c := fakeCalc{}.Calculate(m)
	if max(m.InputTokens, m.PromptTokens) > 100_000 {
		c.InputCost *= 5
		c.CachedCost *= 5
		c.CacheCreationCost *= 5
		c.OutputCost *= 5
		c.TotalCost *= 5
	}
	return c
}

// A tool result carried by a request above the long-context threshold is
// priced at that request's rate, not the base rate its own size would get.
func TestExplainPricesSlicesAtTheirRequestsTier(t *testing.T) {
	path := filepath.Join(t.TempDir(), "L.jsonl")
	writeFile(t, path,
		prompt("go"),
		assistant("r1", 200_000, 0, 10, "t1"),
		result("t1", 4000),
		assistant("r2", 200_000, 1010, 10),
	)
	th, err := parseFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	b := Explain(tierCalc{}, th, 0, len(th.Requests), nil)
	if len(b.Tools) != 1 {
		t.Fatalf("tools = %+v", b.Tools)
	}
	// 1000 tokens written at the 1h rate (2x input), 5x for the long prompt.
	if got, want := b.Tools[0].WriteCost, 1000*2*5/1e6; math.Abs(got-want) > 1e-12 {
		t.Errorf("tool write cost = %v, want %v", got, want)
	}
	if math.Abs(b.Attributed()-b.Total) > 1e-12 {
		t.Errorf("parts %.9f != total %.9f", b.Attributed(), b.Total)
	}
}

func attachment(kind string, chars int) string {
	return `{"type":"attachment","attachment":{"type":"` + kind + `","addedLines":["` + strings.Repeat("x", chars) + `"]}}`
}

// Claude Code can inject its own context (here an MCP tool listing that
// arrived after a small tool result). The growth it causes is not the tool's:
// a result in such a gap is sized from its own characters.
func TestInjectedContextIsNotBlamedOnTools(t *testing.T) {
	path := filepath.Join(t.TempDir(), "I.jsonl")
	writeFile(t, path,
		prompt("go"),
		assistant("r1", 20_000, 0, 10, "t1"),
		result("t1", 2300),
		attachment("deferred_tools_delta", 48_000),
		assistant("r2", 20_000, 17_010, 10, "t2"),
		result("t2", 2300),
		assistant("r3", 37_010, 800, 10),
	)
	th, err := parseFixture(path)
	if err != nil {
		t.Fatal(err)
	}
	sizes := th.ResultTokens()
	// Mixed gap: 2300 chars / 2.3 + 28 framing, not the 17,000-token growth.
	if got := sizes[0]; got != 1028 {
		t.Errorf("t1 = %d tokens, want 1028", got)
	}
	// Clean gap: exactly the context growth (37,810 - 37,010 - 10 output).
	if got := sizes[1]; got != 790 {
		t.Errorf("t2 = %d tokens, want 790", got)
	}
	b := Explain(fakeCalc{}, th, 0, len(th.Requests), nil)
	if math.Abs(b.Attributed()-b.Total) > 1e-12 {
		t.Errorf("parts %.9f != total %.9f", b.Attributed(), b.Total)
	}
}

func loadFixture(path string) (*Session, error) { return LoadSession(filepath.Dir(path), path) }
func parseFixture(path string) (*Thread, error) {
	return parseThread(os.DirFS(filepath.Dir(path)), filepath.Base(path))
}
