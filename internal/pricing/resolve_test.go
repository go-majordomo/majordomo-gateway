package pricing

import "testing"

func TestStripDateSuffix(t *testing.T) {
	cases := []struct {
		in       string
		wantBase string
		wantOK   bool
	}{
		{"gpt-5.4-mini-2026-03-17", "gpt-5.4-mini", true},                 // OpenAI dashed date
		{"claude-haiku-4-5-20251001", "claude-haiku-4-5", true},           // Anthropic YYYYMMDD
		{"gpt-5.4-mini", "gpt-5.4-mini", false},                           // undated
		{"gemini-2.5-flash-lite-001", "gemini-2.5-flash-lite-001", false}, // -001 is not a date
		{"gemini-3.1-flash-lite-preview", "gemini-3.1-flash-lite-preview", false},
	}
	for _, c := range cases {
		base, ok := stripDateSuffix(c.in)
		if base != c.wantBase || ok != c.wantOK {
			t.Errorf("stripDateSuffix(%q) = (%q, %v), want (%q, %v)", c.in, base, ok, c.wantBase, c.wantOK)
		}
	}
}

func TestResolveDatedModels(t *testing.T) {
	s := &Service{
		pricing: map[string]ModelPricing{
			"gpt-5.4-mini":          {InputPricePerMillion: 1},
			"claude-haiku-4-5":      {InputPricePerMillion: 2},
			"gemini-2.5-flash-lite": {InputPricePerMillion: 3},
		},
		aliases: map[string]string{
			"gemini-2.5-flash-lite-001": "gemini-2.5-flash-lite",
		},
	}

	found := []string{
		"gpt-5.4-mini",              // exact
		"gpt-5.4-mini-2026-03-17",   // OpenAI dated -> stripped to canonical
		"claude-haiku-4-5-20251001", // Anthropic dated -> stripped to canonical
		"gemini-2.5-flash-lite-001", // Gemini -> alias
	}
	for _, m := range found {
		if _, ok := s.resolve(m); !ok {
			t.Errorf("resolve(%q) = not found, want found", m)
		}
	}

	if _, ok := s.resolve("totally-unknown-model"); ok {
		t.Errorf("resolve(unknown) = found, want not found")
	}
}
