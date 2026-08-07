package spanid

import (
	"testing"

	"github.com/google/uuid"
)

func TestInteriorSpanIDDeterministic(t *testing.T) {
	a := InteriorSpanID("run_1", "planner/tool:search_db")
	b := InteriorSpanID("run_1", "planner/tool:search_db")
	if a != b {
		t.Fatalf("expected deterministic id, got %s and %s", a, b)
	}
}

func TestInteriorSpanIDDistinctInputs(t *testing.T) {
	cases := [][2]string{
		{"run_1", "planner"},
		{"run_1", "planner/tool:search_db"},
		{"run_2", "planner"},
		{"run_1", ""},
	}
	seen := map[uuid.UUID][2]string{}
	for _, c := range cases {
		id := InteriorSpanID(c[0], c[1])
		if prev, ok := seen[id]; ok {
			t.Fatalf("collision: %v and %v both -> %s", prev, c, id)
		}
		seen[id] = c
	}
}

func TestCanonicalPath(t *testing.T) {
	cases := map[string]string{
		"":                       "",
		"planner":                "planner",
		"/planner/tool":          "planner/tool",
		"planner//tool":          "planner/tool",
		"planner/tool/":          "planner/tool",
		"planner/tool:search_db": "planner/tool:search_db",
	}
	for in, want := range cases {
		if got := CanonicalPath(in); got != want {
			t.Errorf("CanonicalPath(%q) = %q, want %q", in, got, want)
		}
	}
}

// The id derived from a raw path must equal the id derived from its canonical
// form, so capture (which canonicalizes) and any future re-derivation agree.
func TestInteriorSpanIDStableUnderCanonicalization(t *testing.T) {
	raw := "/planner//tool:search_db/"
	canonical := CanonicalPath(raw)
	if InteriorSpanID("run_1", canonical) != InteriorSpanID("run_1", "planner/tool:search_db") {
		t.Fatalf("canonicalized id mismatch for %q", raw)
	}
}

func TestAncestorPaths(t *testing.T) {
	got := AncestorPaths("a/b/c")
	want := []string{"a", "a/b", "a/b/c"}
	if len(got) != len(want) {
		t.Fatalf("AncestorPaths length = %d, want %d (%v)", len(got), len(want), got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("AncestorPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}
	if AncestorPaths("") != nil {
		t.Errorf("AncestorPaths(\"\") should be nil")
	}
}
