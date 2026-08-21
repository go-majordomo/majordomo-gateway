package pricing

import (
	"os"
	"testing"
)

// TestRealCatalogRoutesAllResolve loads the repo's actual model_catalog.json and
// asserts every route option survives parsing. A route whose model has no price
// entry is dropped with only a log warning, so without this the file can look
// correct while a provider silently never routes.
func TestRealCatalogRoutesAllResolve(t *testing.T) {
	data, err := os.ReadFile("../../model_catalog.json")
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	_, _, catalog, err := parseCatalog(data)
	if err != nil {
		t.Fatalf("parseCatalog: %v", err)
	}

	want := map[string]int{
		"deepseek-v4-pro": 7, "kimi-k2.6": 7, "kimi-k3": 7,
		"glm-5.1": 6, "glm-5.2": 6, "inkling": 4,
	}
	for slug, n := range want {
		eps, ok := catalog[slug]
		if !ok {
			t.Errorf("slug %q missing from catalog", slug)
			continue
		}
		if len(eps) != n {
			got := make([]string, len(eps))
			for i, e := range eps {
				got[i] = e.Provider
			}
			t.Errorf("slug %q kept %d of %d options (%v) — a dropped option means a missing price entry", slug, len(eps), n, got)
		}
	}

	// Both new providers must actually appear as routable options.
	for _, p := range []string{"deepinfra", "novita"} {
		found := false
		for _, eps := range catalog {
			for _, e := range eps {
				if e.Provider == p {
					found = true
				}
			}
		}
		if !found {
			t.Errorf("provider %q is in no route", p)
		}
	}

	// deepinfra's base URL must carry its version segment; routed traffic reads
	// this value, not the config default.
	for _, e := range catalog["glm-5.2"] {
		if e.Provider == "deepinfra" && e.BaseURL != "https://api.deepinfra.com/v1/openai" {
			t.Errorf("deepinfra catalog base URL = %q, want the versioned form", e.BaseURL)
		}
	}
}
