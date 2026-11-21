package agentusage

import (
	"math"
	"testing"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

func TestFreshToolSavingsMatchSessionSavings(t *testing.T) {
	calc := pricing.NewService("", "../../model_catalog.json", "../../model_aliases.json", time.Hour)
	defer calc.Close()
	run := func(n int) Report {
		s := Session{ID: "S", Source: "codex", Requests: []Request{
			{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{Model: "gpt-6.2-sol", CacheReadTokens: 9000, InputTokens: n, OutputTokens: 20}, Results: []Result{{ID: "1", Name: "mcp__demo__lookup", Tokens: n}}},
		}}
		r, imp, err := Analyze(&s, calc, false, "", nil)
		if err != nil {
			t.Fatal(err)
		}
		charge := imp.ToolSpans[0].Charges[0]
		if charge.Cached != 0 || charge.Input != n {
			t.Fatalf("imported charge = %+v", charge)
		}
		return r
	}
	before, after := run(1000), run(500)
	saved := before.Turns[0].Main.ToolsTotal - after.Turns[0].Main.ToolsTotal
	if math.Abs(saved*1000-1) > 1e-9 || math.Abs(before.Total-after.Total-saved) > 1e-10 {
		t.Fatalf("tool savings %g, session savings %g", saved, before.Total-after.Total)
	}
}

func TestNestedChildImportPaths(t *testing.T) {
	session := func(id string) *Session {
		return &Session{ID: id, Source: "codex", Requests: []Request{{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{RequestID: "one", Model: "gpt-6.2-sol"}}}}
	}
	root, child, grandchild := session("root"), session("child"), session("grandchild")
	root.Children = []*Session{child}
	child.Children = []*Session{grandchild}
	_, payload, err := Analyze(root, nil, false, "run", nil)
	if err != nil {
		t.Fatal(err)
	}
	last := payload.Requests[2]
	if last.RequestID != "grandchild/one" || last.SpanPath != "run/turn-1/agent:child/turn-1/agent:grandchild" {
		t.Fatalf("nested request: %+v", last)
	}
}
