package repositories

import (
	"math"
	"testing"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/spanid"
)

func approx(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func leaf(spanPath, spanName string, cost float64, t time.Time) runLeafRow {
	return runLeafRow{
		SpanPath:    spanPath,
		SpanName:    spanName,
		Provider:    "openai",
		Model:       "gpt-4.1",
		RequestedAt: t,
		RespondedAt: t.Add(time.Second),
		TotalCost:   cost,
	}
}

func findChild(n *models.RunNode, name string) *models.RunNode {
	for _, c := range n.Children {
		if c.Name == name {
			return c
		}
	}
	return nil
}

// A flat run (no span_path) hangs every call directly under the root.
func TestAssembleRunTreeFlat(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rows := []runLeafRow{
		leaf("", "call-1", 0.01, base),
		leaf("", "call-2", 0.02, base.Add(time.Second)),
	}
	tree := assembleRunTree("run_flat", rows, false)

	if tree.RequestCount != 2 {
		t.Errorf("request_count = %d, want 2", tree.RequestCount)
	}
	if !approx(tree.TotalCost, 0.03) {
		t.Errorf("total_cost = %v, want 0.03", tree.TotalCost)
	}
	if len(tree.Root.Children) != 2 {
		t.Fatalf("root children = %d, want 2", len(tree.Root.Children))
	}
	for _, c := range tree.Root.Children {
		if c.Kind != models.RunNodeKindLLM {
			t.Errorf("child kind = %q, want llm", c.Kind)
		}
	}
}

// A multi-level run groups leaves under synthesized step nodes and rolls cost up.
func TestAssembleRunTreeNestedRollup(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rows := []runLeafRow{
		leaf("planner", "route", 0.004, base),
		leaf("planner/tool:search_db", "summarize", 0.021, base.Add(time.Second)),
		leaf("planner/tool:search_db", "rank", 0.027, base.Add(2*time.Second)),
	}
	tree := assembleRunTree("run_nested", rows, false)

	if !approx(tree.TotalCost, 0.052) {
		t.Errorf("total_cost = %v, want 0.052", tree.TotalCost)
	}
	if tree.RequestCount != 3 {
		t.Errorf("request_count = %d, want 3", tree.RequestCount)
	}

	planner := findChild(tree.Root, "planner")
	if planner == nil {
		t.Fatal("missing planner step node")
	}
	if planner.Kind != models.RunNodeKindStep {
		t.Errorf("planner kind = %q, want step", planner.Kind)
	}
	if planner.SelfCost != 0 {
		t.Errorf("planner self_cost = %v, want 0 (interior nodes own no cost)", planner.SelfCost)
	}
	// planner subtree = route (0.004) + search_db subtree (0.048)
	if !approx(planner.TotalCost, 0.052) {
		t.Errorf("planner total_cost = %v, want 0.052", planner.TotalCost)
	}

	search := findChild(planner, "tool:search_db")
	if search == nil {
		t.Fatal("missing tool:search_db step node")
	}
	if !approx(search.TotalCost, 0.048) {
		t.Errorf("tool:search_db total_cost = %v, want 0.048", search.TotalCost)
	}
	if search.RequestCount != 2 {
		t.Errorf("tool:search_db request_count = %d, want 2", search.RequestCount)
	}

	// The synthesized step id must match the deterministic derivation, so a later
	// Tier 2 tool-span ingest joins onto the same node.
	if search.SpanID != spanid.InteriorSpanID("run_nested", "planner/tool:search_db") {
		t.Error("tool:search_db span id does not match deterministic derivation")
	}
}

// The run label is the earliest call's top-level step name.
func TestAssembleRunTreeLabel(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rows := []runLeafRow{leaf("planner/tool:search_db", "summarize", 0.01, base)}
	tree := assembleRunTree("run_label", rows, false)
	if tree.Root.Name != "planner" {
		t.Errorf("root label = %q, want planner", tree.Root.Name)
	}
}

// Non-canonical paths (leading/duplicate separators) still resolve to the same nodes.
func TestAssembleRunTreeCanonicalizesPaths(t *testing.T) {
	base := time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)
	rows := []runLeafRow{
		leaf("planner/tool", "a", 0.01, base),
		leaf("/planner//tool/", "b", 0.02, base.Add(time.Second)),
	}
	tree := assembleRunTree("run_canon", rows, false)
	planner := findChild(tree.Root, "planner")
	if planner == nil {
		t.Fatal("missing planner node")
	}
	tool := findChild(planner, "tool")
	if tool == nil {
		t.Fatal("missing tool node")
	}
	if len(tool.Children) != 2 {
		t.Errorf("tool children = %d, want 2 (both leaves under one canonical node)", len(tool.Children))
	}
}
