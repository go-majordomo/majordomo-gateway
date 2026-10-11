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
		"kimi-k3": 3, "glm-5.2": 2, "glm-5.3": 3, "glm-5.3-flash": 2,
		"deepseek-v4-pro-0813": 2, "deepseek-v4.1-flash": 2,
		"qwen3.8-2.4t-a95b": 2, "inkling": 2,
	}
	if len(catalog) != len(want) {
		t.Errorf("catalog has %d routable slugs, want %d", len(catalog), len(want))
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

	// Only the supported open-weight hosts may appear as routable options.
	routable := map[string]bool{"fireworks": true, "together": true, "baseten": true}
	for slug, eps := range catalog {
		for _, e := range eps {
			if !routable[e.Provider] {
				t.Errorf("slug %q routes to unsupported provider %q", slug, e.Provider)
			}
		}
	}
}
