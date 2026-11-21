package services

import (
	"context"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
)

// transcript writes a one-thread Claude Code transcript: r1 calls tool t1,
// r2 carries its result (writing `write` tokens to the cache, `write1h` of them
// with the 1-hour TTL), r3 and r4 re-read it.
func transcript(t *testing.T, read, write, write1h int) *claudecode.Thread {
	t.Helper()
	req := func(id string, read, write, write1h int, tool string) string {
		blocks := `{"type":"text","text":"x"}`
		if tool != "" {
			blocks += `,{"type":"tool_use","id":"` + tool + `","name":"mcp__crm__get_leads","input":{}}`
		}
		return `{"type":"assistant","sessionId":"S","requestId":"` + id + `","timestamp":"2026-11-17T10:00:00Z","message":{"model":"claude-haiku-5-5","content":[` +
			blocks + `],"usage":{"input_tokens":2,"output_tokens":10,"cache_read_input_tokens":` + strconv.Itoa(read) +
			`,"cache_creation_input_tokens":` + strconv.Itoa(write) + `,"cache_creation":{"ephemeral_1h_input_tokens":` + strconv.Itoa(write1h) + `}}}}`
	}
	lines := []string{
		`{"type":"user","timestamp":"2026-11-17T10:00:00Z","message":{"role":"user","content":"go"}}`,
		req("r1", read, 0, 0, "t1"),
		`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":"` + strings.Repeat("a", 2300) + `"}]}}`,
		req("r2", read, write, write1h, ""),
		req("r3", read+write+12, 0, 0, ""),
		req("r4", read+write+24, 0, 0, ""),
	}
	path := filepath.Join(t.TempDir(), "S.jsonl")
	if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	session, err := claudecode.LoadSession(filepath.Dir(path), path)
	if err != nil {
		t.Fatal(err)
	}
	return session.Main
}

// explain (local) and import (gateway) must report the same cost for every
// tool result: uncached, 5-minute and 1-hour writes, and long-context re-reads.
func TestExplainAndImportAgreeOnToolCost(t *testing.T) {
	cases := []struct {
		name                 string
		read, write, write1h int
		calc                 claudecode.Calculator
	}{
		{"uncached result", 20_000, 0, 0, fakeCalc{}},
		{"5-minute write", 20_000, 1012, 0, fakeCalc{}},
		{"1-hour write", 20_000, 1012, 1012, fakeCalc{}},
		{"long-context prompt", 150_000, 1012, 1012, tierCalc{}},
	}
	for _, c := range cases {
		th := transcript(t, c.read, c.write, c.write1h)
		local := claudecode.Explain(c.calc, th, 0, len(th.Requests), nil).ToolsTotal()
		s := &claudecode.Session{ID: "S", Main: th}
		imp := claudecode.BuildImport(s, "", nil)
		res, err := NewImportService(&fakeImportRepo{}, c.calc).ImportClaudeCode(context.Background(), uuid.New(), &imp)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if local == 0 || math.Abs(local-res.ToolCost) > 1e-12 {
			t.Errorf("%s: explain %.9f, import %.9f", c.name, local, res.ToolCost)
		}
	}
}
