package agentusage

import (
	"context"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/klauspost/compress/zstd"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type calculator struct{}

func (calculator) Calculate(m *models.UsageMetrics) models.Cost {
	if m.Model == "unknown" {
		return models.Cost{}
	}
	rate := 1.
	if m.Model == "second" {
		rate = 2
	}
	if max(m.InputTokens, m.PromptTokens) > 150 { // a long-context tier
		rate *= 5
	}
	input := float64(m.InputTokens-m.CachedTokens) * rate
	hit := float64(m.CachedTokens) * rate / 10
	output := float64(m.OutputTokens) * 3 * rate
	return models.Cost{ModelAliasFound: true, InputCost: input, CachedCost: hit, OutputCost: output, TotalCost: input + hit + output}
}

func TestCodexRollouts(t *testing.T) {
	dir := t.TempDir()
	rows := []string{
		`"session_meta", "payload":{"id":"sid","cwd":"/project","source":"cli"}`,
		`"turn_context", "payload":{"model":"first","turn_id":"A"}`,
		`"response_item", "payload":{"type":"additional_tools","tools":[{"name":"mcp__maps","type":"namespace"}]}`,
		`"event_msg", "payload":{"type":"user_message","message":"private question"}`,
		`"response_item", "payload":{"type":"function_call","namespace":"mcp__maps","name":"lookup","call_id":"call1","arguments":"private argument"}`,
		`"token_usage_record", "payload":{"response_id":"r1","usage":{"input_tokens":100,"cached_input_tokens":90,"output_tokens":10,"reasoning_output_tokens":5},"thread_token_usage":{"total_tokens":110}}`,
		`"response_item", "payload":{"type":"function_call_output","call_id":"call1","output":"private result"}`,
		`"event_msg", "payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":110},"last_token_usage":{"input_tokens":100,"output_tokens":10}}}`,
		`"event_msg", "payload":{"type":"token_count","info":{"total_token_usage":{"total_tokens":240},"last_token_usage":{"input_tokens":120,"output_tokens":10,"cached_input_tokens":60}}}`,
		`"turn_context", "payload":{"model":"second","turn_id":"B"}`,
		`"token_usage_record", "payload":{"response_id":"r3","usage":{"input_tokens":160,"output_tokens":5},"thread_token_usage":{"total_tokens":405}}`,
		`"compacted","payload":{}`,
		`"token_usage_record","payload":{"response_id":"r4","usage":{"input_tokens":200,"output_tokens":1},"thread_token_usage":{"total_tokens":606}}`,
	}
	var transcript strings.Builder
	for _, line := range rows {
		transcript.WriteString(`{"timestamp":"2026-11-01T12:00:00Z","type":` + line + "}\n")
	}
	transcript.WriteString("{")
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := enc.Close(); err != nil {
			t.Error(err)
		}
	})
	if err = os.WriteFile(filepath.Join(dir, "rollout.jsonl.zst"), enc.EncodeAll([]byte(transcript.String()), nil), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), "codex", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.Find("/project", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(context.Background(), entries[0])
	if err != nil {
		t.Fatal(err)
	}
	r, imp, err := Analyze(s, calculator{}, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if r.Input != 580 || r.OutputTokens != 26 || r.Cached != 150 || len(r.Turns) != 2 || len(imp.Requests) != 4 || len(imp.ToolSpans) != 1 {
		t.Fatalf("unexpected report %+v", r)
	}
	span := imp.ToolSpans[0]
	if span.ToolName != "mcp__maps__lookup" || len(span.Charges) != 2 || span.ResultTokens != 4 {
		t.Fatalf("wrong tool %+v", span)
	}
	if imp.Requests[0].InputTokens != 10 || len(r.Turns[0].Main.Definitions) != 1 {
		t.Fatal("cache or definition accounting")
	}
	checkTotals(t, r, imp)
	data, _ := json.Marshal(imp)
	if strings.Contains(string(data), "private") {
		t.Fatal("import included transcript content")
	}
	if cmp := Compare(r, r); cmp.Delta != 0 || cmp.BeforeRequests != 4 {
		t.Fatalf("comparison %+v", cmp)
	}
}

func checkTotals(t *testing.T, r Report, imp models.ClaudeCodeImport) {
	t.Helper()
	cost := 0.
	for _, c := range r.Components() {
		cost += c
	}
	if math.Abs(cost-r.Total) > 1e-8 {
		t.Fatalf("components %g total %g", cost, r.Total)
	}
	var local, imported float64
	for _, tr := range r.Turns {
		local += tr.Main.ToolsTotal
		for _, sub := range tr.Subagents {
			local += sub.ToolsTotal
		}
	}
	for _, s := range imp.ToolSpans {
		for _, c := range s.Charges {
			imported += calculator{}.Calculate(&models.UsageMetrics{Model: c.Model, InputTokens: c.Input, CachedTokens: c.Cached, PromptTokens: c.Prompt}).TotalCost
		}
	}
	if math.Abs(imported-local) > 1e-8 {
		t.Fatalf("local tool cost %g import %g", local, imported)
	}
}

func TestCursorDesktop(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.vscdb")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE cursorDiskKV(key TEXT PRIMARY KEY,value TEXT)"); err != nil {
		t.Fatal(err)
	}
	records := map[string]string{
		"composerData:sid": `{"createdAt":1000,"trackedGitRepos":[{"repoPath":"/repo"}],"modelConfig":{"modelName":"first"},"fullConversationHeadersOnly":[{"bubbleId":"u"},{"bubbleId":"a"},{"bubbleId":"b"}],"promptTokenBreakdown":{"totalUsedTokens":77,"categories":[{"id":"mcp","estimatedTokens":7}]}}`,
		"bubbleId:sid:u":   `{"bubbleId":"u","type":1,"text":"private question"}`,
		"bubbleId:sid:a":   `{"bubbleId":"a","type":2,"tokenCount":{"inputTokens":0,"outputTokens":0},"toolFormerData":{"name":"mcp-demo-lookup","toolCallId":"abc","rawArgs":"{\"providerIdentifier\":\"demo\",\"toolName\":\"lookup\"}","result":"private result"}}`,
		"bubbleId:sid:b":   `{"bubbleId":"b","type":2,"text":"private answer","tokenCount":{"inputTokens":77,"cachedInputTokens":40,"outputTokens":2}}`,
	}
	for k, v := range records {
		if _, err = db.Exec("INSERT INTO cursorDiskKV VALUES(?,?)", k, v); err != nil {
			t.Fatal(err)
		}
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), "cursor", path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	entries, err := store.Find("/repo", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(context.Background(), entries[0])
	if err != nil {
		t.Fatal(err)
	}
	r, imp, err := Analyze(s, calculator{}, true, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Estimated || imp.Metadata["usage_basis"] != "estimated" || r.Input != 81 || r.Cached != 40 || len(imp.ToolSpans) != 1 || imp.ToolSpans[0].ToolName != "mcp__demo__lookup" {
		t.Fatalf("wrong Cursor attribution %+v", imp)
	}
	checkTotals(t, r, imp)
}

// writeCursorCLIStore writes a Cursor CLI chat store for agent "sid" under dir.
func writeCursorCLIStore(t *testing.T, dir string) {
	t.Helper()
	path := filepath.Join(dir, "workspace", "sid", "store.db")
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("CREATE TABLE blobs(id TEXT PRIMARY KEY,data BLOB); CREATE TABLE meta(key TEXT PRIMARY KEY,value TEXT)"); err != nil {
		t.Fatal(err)
	}
	var root []byte
	for i, msg := range []string{
		`{"role":"system","content":"instructions"}`,
		`{"role":"user","content":[{"type":"text","text":"private"}]}`,
		`{"role":"assistant","content":[{"type":"tool-call","toolCallId":"one","toolName":"mcp__abc__test"}]}`,
		`{"role":"tool","content":[{"type":"tool-result","toolCallId":"one","toolName":"mcp__abc__test","result":"private result"}]}`,
		`{"role":"assistant","timestamp":"2026-11-18T12:00:00Z","content":"done"}`,
	} {
		id := []byte{byte(i)}
		root = protowire.AppendBytes(protowire.AppendTag(root, 1, protowire.BytesType), id)
		if _, err = db.Exec("INSERT INTO blobs VALUES(?,?)", hex.EncodeToString(id), []byte(msg)); err != nil {
			t.Fatal(err)
		}
	}
	root = protowire.AppendString(protowire.AppendTag(root, 9, protowire.BytesType), "file:///repo")
	if _, err = db.Exec("INSERT INTO blobs VALUES('root',?),('orphan','invalid stale blob')", root); err != nil {
		t.Fatal(err)
	}
	if _, err = db.Exec("INSERT INTO meta VALUES('0',?)", hex.EncodeToString([]byte(`{"agentId":"sid","latestRootBlobId":"root","createdAt":1000,"lastUsedModel":"first"}`))); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestCursorCLIContextOrder(t *testing.T) {
	dir := t.TempDir()
	writeCursorCLIStore(t, dir)
	store, err := Open(context.Background(), "cursor", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	matches, err := store.Find("/repo", "", time.Time{})
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.Load(context.Background(), matches[0])
	if err != nil {
		t.Fatal(err)
	}
	r, imp, err := Analyze(s, calculator{}, false, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Estimated || r.Requests() != 2 || len(imp.ToolSpans) != 1 || imp.ToolSpans[0].ToolName != "mcp__abc__test" {
		t.Fatalf("wrong CLI report %+v", r)
	}
	if !r.EstimatedTime || imp.Metadata["time_basis"] != "session_start" || imp.Requests[1].OccurredAt.UTC().Format(time.RFC3339) != "2026-11-18T12:00:00Z" {
		t.Fatalf("request times: %+v", imp.Requests)
	}
	checkTotals(t, r, imp)
}

func TestProbeClients(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX executable fixture")
	}
	dir := t.TempDir()
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	config := filepath.Join(dir, "mcp.json")
	if err := os.WriteFile(config, []byte(`{"mcpServers":{"demo":{"command":"example"}}}`), 0600); err != nil {
		t.Fatal(err)
	}
	scripts := map[string]string{
		"codex": `n=100; if grep -q example "$CODEX_HOME/config.toml"; then n=130; fi
 echo "{\"type\":\"turn.completed\",\"usage\":{\"input_tokens\":$n}}"`,
		"cursor-agent": `n=100; if grep -q example "$CURSOR_CONFIG_DIR/mcp.json"; then n=130; fi
 echo "{\"type\":\"result\",\"usage\":{\"input_tokens\":$n}}"`,
	}
	for name, script := range scripts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("#!/bin/sh\n"+script), 0700); err != nil {
			t.Fatal(err)
		}
	}
	for _, source := range []string{"codex", "cursor"} {
		p, err := Probe(context.Background(), source, "first", config)
		if err != nil {
			t.Fatal(err)
		}
		if p.Baseline != 100 || p.Alone["demo"] != 30 || p.Combined != 30 {
			t.Fatal(p)
		}
		data, _ := json.Marshal(p)
		path := filepath.Join(dir, "probe.json")
		if err = os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		s := Session{Source: source, Requests: []Request{{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{Model: "first"}}, {ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{Model: "second"}}}}
		if err = s.ApplyMeasurement(path); err != nil {
			t.Fatal(err)
		}
		if s.Requests[0].Definitions["demo"] != 30 || s.Requests[1].Definitions != nil {
			t.Fatal(s.Requests)
		}
	}
}

// One unreadable Cursor CLI store must not hide the healthy ones: it is
// skipped and reported, and the healthy session is still found.
func TestCursorCLISkipsUnreadableStore(t *testing.T) {
	dir := t.TempDir()
	writeCursorCLIStore(t, dir)
	bad := filepath.Join(dir, "workspace", "broken", "store.db")
	if err := os.MkdirAll(filepath.Dir(bad), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bad, []byte("not a database"), 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(context.Background(), "cursor", dir)
	if err != nil {
		t.Fatalf("open with one broken store: %v", err)
	}
	defer store.Close()
	if len(store.Skipped) != 1 || !strings.Contains(store.Skipped[0], "broken") {
		t.Errorf("skipped = %v, want the broken store", store.Skipped)
	}
	if matches, err := store.Find("", "sid", time.Time{}); err != nil || len(matches) != 1 {
		t.Fatalf("healthy session by id: %v %v", matches, err)
	}
	_, err = store.Find("", "missing", time.Time{})
	if err == nil || !strings.Contains(err.Error(), "could not be read") {
		t.Errorf("not-found error should mention skipped stores: %v", err)
	}
}

// tieredCalc is $1/M input and $0.10/M cached, 5x for a prompt above 100K.
type tieredCalc struct{}

func (tieredCalc) Calculate(m *models.UsageMetrics) models.Cost {
	rate := 1.0
	if max(m.InputTokens, m.PromptTokens) > 100_000 {
		rate = 5
	}
	in := float64(m.InputTokens-m.CachedTokens) * rate / 1e6
	hit := float64(m.CachedTokens) * 0.1 * rate / 1e6
	return models.Cost{ModelAliasFound: true, InputCost: in, CachedCost: hit, TotalCost: in + hit}
}

// The projection charges only the schema tokens, at the tier of the prompt
// they ride in: 1,000 tokens x 1,000 requests at a 105K prompt cost 5x.
func TestProjectPricesAtThePromptTier(t *testing.T) {
	defs := map[string]int{"crm": 1000}
	base := Project(tieredCalc{}, "codex", "m", defs, 0, 1000)
	long := Project(tieredCalc{}, "codex", "m", defs, 105_000, 1000)
	if got := base.Servers[0].Uncached; math.Abs(got-1.0) > 1e-9 {
		t.Errorf("base uncached = %v, want 1.00", got)
	}
	if got := long.Servers[0].Uncached; math.Abs(got-5.0) > 1e-9 {
		t.Errorf("long-prompt uncached = %v, want 5.00", got)
	}
	if long.PromptTokens != 105_000 || !strings.Contains(base.Note, "no prompt size") {
		t.Errorf("tier assumption not reported: %+v / %q", long, base.Note)
	}
}
