package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
)

// --- fixtures -------------------------------------------------------------

type fakeCatalog map[string][]pricing.Endpoint

func (f fakeCatalog) Candidates(model string) ([]pricing.Endpoint, bool) {
	eps, ok := f[model]
	return eps, ok
}

type fakeHealth []models.EndpointHealth

func (f fakeHealth) GetEndpointHealth(_ context.Context, _ time.Time) ([]models.EndpointHealth, error) {
	return f, nil
}

// fakeKeys treats every provider in the set as credentialed.
type fakeKeys map[string]bool

func (f fakeKeys) HasKey(_ context.Context, provider string) (bool, error) {
	return f[provider], nil
}

// fakePricer maps (provider, model) -> price. Missing pairs return ok=false so
// the router's unknown-price deprioritization can be exercised.
type fakePricer map[string]pricing.ModelPricing

func (f fakePricer) Price(provider, model string) (pricing.ModelPricing, bool) {
	p, ok := f[provider+"\x00"+model]
	return p, ok
}

func (f fakePricer) set(provider, model string, in, out float64) fakePricer {
	f[provider+"\x00"+model] = pricing.ModelPricing{InputPricePerMillion: in, OutputPricePerMillion: out}
	return f
}

func ep(provider, modelID string) pricing.Endpoint {
	return pricing.Endpoint{
		Provider:        provider,
		BaseURL:         "https://" + provider + ".example",
		ProviderModelID: modelID,
		Region:          "us",
	}
}

func epWithPolicy(provider, modelID string, zdr bool, dataCollection string) pricing.Endpoint {
	e := ep(provider, modelID)
	e.ZDR = zdr
	e.DataCollection = dataCollection
	return e
}

// newRouter builds a router with a deterministic rand source (returns fixed r).
func newRouter(cat CandidateSource, h HealthStore, k KeyAvailability, p Pricer, r float64) *ProviderRouter {
	return &ProviderRouter{
		catalog:   cat,
		health:    h,
		keys:      k,
		pricer:    p,
		randFloat: func() float64 { return r },
	}
}

// --- tests ----------------------------------------------------------------

func TestRoute_NotInCatalog(t *testing.T) {
	rt := newRouter(fakeCatalog{}, fakeHealth{}, fakeKeys{"fireworks": true}, fakePricer{}, 0)
	dec, err := rt.Route(context.Background(), "gpt-5", DataPolicy{})
	if err != nil || dec != nil {
		t.Fatalf("expected (nil, nil) for uncatalogued model, got (%v, %v)", dec, err)
	}
}

func TestRoute_NoCredentialedEndpoint(t *testing.T) {
	cat := fakeCatalog{"deepseek-v4-pro-0813": {ep("fireworks", "fw/ds"), ep("together", "tg/ds")}}
	rt := newRouter(cat, fakeHealth{}, fakeKeys{}, fakePricer{}, 0) // no keys at all
	dec, err := rt.Route(context.Background(), "deepseek-v4-pro-0813", DataPolicy{})
	if dec != nil {
		t.Fatalf("expected nil decision, got %+v", dec)
	}
	if !errors.Is(err, ErrNoUsableEndpoint) {
		t.Fatalf("expected ErrNoUsableEndpoint, got %v", err)
	}
}

func TestRoute_CostWeightingPrefersCheaper(t *testing.T) {
	cat := fakeCatalog{"deepseek-v4-pro-0813": {ep("together", "tg/ds"), ep("fireworks", "fw/ds")}}
	pricer := fakePricer{}.set("fireworks", "fw/ds", 1, 1).set("together", "tg/ds", 2, 2)
	// Both credentialed and healthy (no health rows → all pass). r=0 selects the
	// first bucket, which is the cheapest after the deterministic cost sort.
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"fireworks": true, "together": true}, pricer, 0)
	dec, err := rt.Route(context.Background(), "deepseek-v4-pro-0813", DataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "fireworks" {
		t.Fatalf("expected cheaper 'fireworks', got %q (%s)", dec.Provider, dec.Reason)
	}
	if dec.ProviderModelID != "fw/ds" || dec.BaseURL != "https://fireworks.example" {
		t.Errorf("decision did not carry the chosen endpoint's fields: %+v", dec)
	}
}

func TestRoute_HealthGateExcludesHighError(t *testing.T) {
	cat := fakeCatalog{"deepseek-v4-pro-0813": {ep("fireworks", "fw/ds"), ep("together", "tg/ds")}}
	pricer := fakePricer{}.set("fireworks", "fw/ds", 1, 1).set("together", "tg/ds", 2, 2)
	// Cheaper fireworks is unhealthy (enough samples, high error rate) → dropped,
	// so the pricier-but-healthy together must win despite r=0.
	health := fakeHealth{
		{Provider: "fireworks", Model: "fw/ds", SampleCount: 100, ErrorRate: 0.5},
		{Provider: "together", Model: "tg/ds", SampleCount: 100, ErrorRate: 0.0},
	}
	rt := newRouter(cat, health, fakeKeys{"fireworks": true, "together": true}, pricer, 0)
	dec, err := rt.Route(context.Background(), "deepseek-v4-pro-0813", DataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "together" {
		t.Fatalf("expected healthy 'together', got %q (%s)", dec.Provider, dec.Reason)
	}
}

func TestRoute_AllUnhealthyFallsBackToCheapest(t *testing.T) {
	cat := fakeCatalog{"deepseek-v4-pro-0813": {ep("fireworks", "fw/ds"), ep("together", "tg/ds")}}
	pricer := fakePricer{}.set("fireworks", "fw/ds", 1, 1).set("together", "tg/ds", 2, 2)
	health := fakeHealth{
		{Provider: "fireworks", Model: "fw/ds", SampleCount: 100, ErrorRate: 0.9},
		{Provider: "together", Model: "tg/ds", SampleCount: 100, ErrorRate: 0.9},
	}
	rt := newRouter(cat, health, fakeKeys{"fireworks": true, "together": true}, pricer, 0)
	dec, err := rt.Route(context.Background(), "deepseek-v4-pro-0813", DataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "fireworks" {
		t.Fatalf("expected fallback to cheapest 'fireworks', got %q", dec.Provider)
	}
	if !strings.Contains(dec.Reason, "unhealthy") {
		t.Errorf("expected degraded reason, got %q", dec.Reason)
	}
}

func TestRoute_LowSampleEndpointNotCondemned(t *testing.T) {
	cat := fakeCatalog{"deepseek-v4-pro-0813": {ep("fireworks", "fw/ds")}}
	pricer := fakePricer{}.set("fireworks", "fw/ds", 1, 1)
	// High error rate but below the sample threshold → must still be considered healthy.
	health := fakeHealth{{Provider: "fireworks", Model: "fw/ds", SampleCount: 3, ErrorRate: 1.0}}
	rt := newRouter(cat, health, fakeKeys{"fireworks": true}, pricer, 0)
	dec, err := rt.Route(context.Background(), "deepseek-v4-pro-0813", DataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "fireworks" || strings.Contains(dec.Reason, "unhealthy") {
		t.Fatalf("low-sample endpoint should pass the health gate, got %q (%s)", dec.Provider, dec.Reason)
	}
}

func TestRoute_DeterministicTieBreakOnEqualPrice(t *testing.T) {
	cat := fakeCatalog{"m": {ep("bbb", "b/m"), ep("aaa", "a/m")}}
	pricer := fakePricer{}.set("aaa", "a/m", 1, 1).set("bbb", "b/m", 1, 1)
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"aaa": true, "bbb": true}, pricer, 0)
	// Equal price: the sort tie-break orders by provider name, so r=0 → "aaa".
	for i := 0; i < 5; i++ {
		dec, err := rt.Route(context.Background(), "m", DataPolicy{})
		if err != nil {
			t.Fatal(err)
		}
		if dec.Provider != "aaa" {
			t.Fatalf("iteration %d: expected stable 'aaa', got %q", i, dec.Provider)
		}
	}
}

func TestRoute_UnknownPriceDeprioritizedButSelectable(t *testing.T) {
	cat := fakeCatalog{"m": {ep("priced", "p/m"), ep("unpriced", "u/m")}}
	// Only 'priced' has a price; 'unpriced' gets the sentinel → 'priced' wins.
	pricer := fakePricer{}.set("priced", "p/m", 5, 5)
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"priced": true, "unpriced": true}, pricer, 0)
	dec, err := rt.Route(context.Background(), "m", DataPolicy{})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "priced" {
		t.Fatalf("expected priced endpoint to be preferred, got %q", dec.Provider)
	}

	// When the unpriced endpoint is the only credentialed candidate, it is still served.
	rtOnly := newRouter(fakeCatalog{"m": {ep("unpriced", "u/m")}}, fakeHealth{}, fakeKeys{"unpriced": true}, fakePricer{}, 0)
	if dec, err := rtOnly.Route(context.Background(), "m", DataPolicy{}); err != nil || dec.Provider != "unpriced" {
		t.Fatalf("sole unpriced endpoint must still be served, got (%v, %v)", dec, err)
	}
}

func TestRoute_DataPolicyRequireZDR(t *testing.T) {
	cat := fakeCatalog{"m": {
		epWithPolicy("fireworks", "fw", false, "allow"), // cheaper, not ZDR
		epWithPolicy("together", "tg", true, "deny"),    // pricier, ZDR
	}}
	pricer := fakePricer{}.set("fireworks", "fw", 1, 1).set("together", "tg", 2, 2)
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"fireworks": true, "together": true}, pricer, 0)

	// No policy → cheaper fireworks wins.
	if dec, _ := rt.Route(context.Background(), "m", DataPolicy{}); dec.Provider != "fireworks" {
		t.Fatalf("no policy: expected fireworks, got %q", dec.Provider)
	}
	// RequireZDR → only together qualifies, even though pricier and r=0.
	dec, err := rt.Route(context.Background(), "m", DataPolicy{RequireZDR: true})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "together" {
		t.Fatalf("RequireZDR: expected together, got %q (%s)", dec.Provider, dec.Reason)
	}
}

func TestRoute_DataPolicyRequireNoCollection(t *testing.T) {
	cat := fakeCatalog{"m": {
		epWithPolicy("fireworks", "fw", false, "allow"),
		epWithPolicy("together", "tg", false, "deny"),
	}}
	pricer := fakePricer{}.set("fireworks", "fw", 1, 1).set("together", "tg", 2, 2)
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"fireworks": true, "together": true}, pricer, 0)

	dec, err := rt.Route(context.Background(), "m", DataPolicy{RequireNoDataCollection: true})
	if err != nil {
		t.Fatal(err)
	}
	if dec.Provider != "together" {
		t.Fatalf("RequireNoDataCollection: expected together (deny), got %q", dec.Provider)
	}
}

func TestRoute_DataPolicyNoCompliantEndpoint(t *testing.T) {
	cat := fakeCatalog{"m": {
		epWithPolicy("fireworks", "fw", false, "allow"),
		epWithPolicy("together", "tg", false, "allow"),
	}}
	pricer := fakePricer{}.set("fireworks", "fw", 1, 1).set("together", "tg", 2, 2)
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"fireworks": true, "together": true}, pricer, 0)

	// Credentialed endpoints exist but none is ZDR → ErrNoCompliantEndpoint,
	// distinct from ErrNoUsableEndpoint (which means no credential).
	_, err := rt.Route(context.Background(), "m", DataPolicy{RequireZDR: true})
	if !errors.Is(err, ErrNoCompliantEndpoint) {
		t.Fatalf("expected ErrNoCompliantEndpoint, got %v", err)
	}
}

func TestRoute_CarriesEndpointRequestDefaults(t *testing.T) {
	withDefaults := ep("together", "tg/m")
	withDefaults.RequestDefaults = map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"low"`)}
	cat := fakeCatalog{"m": {withDefaults}}
	rt := newRouter(cat, fakeHealth{}, fakeKeys{"together": true}, fakePricer{}, 0)

	dec, err := rt.Route(context.Background(), "m", DataPolicy{})
	if err != nil || dec == nil {
		t.Fatalf("Route: dec=%v err=%v", dec, err)
	}
	if got := string(dec.RequestDefaults["reasoning_effort"]); got != `"low"` {
		t.Errorf("RequestDefaults[reasoning_effort] = %s, want \"low\"", got)
	}
}

func TestRewriteRoutedRequest(t *testing.T) {
	low := map[string]json.RawMessage{"reasoning_effort": json.RawMessage(`"low"`)}

	tests := []struct {
		name        string
		body        string
		defaults    map[string]json.RawMessage
		wantApplied []string
		want        map[string]string // field -> raw JSON; "" means the field must be absent
	}{
		{
			name:        "default added when the caller sets no reasoning field",
			body:        `{"model":"glm-5.3","messages":[]}`,
			defaults:    low,
			wantApplied: []string{"reasoning_effort"},
			want:        map[string]string{"model": `"tg/m"`, "reasoning_effort": `"low"`, "messages": `[]`},
		},
		{
			name:     "caller's value for the same field wins",
			body:     `{"model":"glm-5.3","reasoning_effort":"high"}`,
			defaults: low,
			want:     map[string]string{"reasoning_effort": `"high"`},
		},
		{
			name:     "explicit null counts as set",
			body:     `{"model":"glm-5.3","reasoning_effort":null}`,
			defaults: low,
			want:     map[string]string{"reasoning_effort": `null`},
		},
		{
			name:     "a different reasoning field from the caller withholds the default",
			body:     `{"model":"glm-5.3","thinking":{"type":"enabled"}}`,
			defaults: low,
			want:     map[string]string{"reasoning_effort": "", "thinking": `{"type":"enabled"}`},
		},
		{
			name: "caller reasoning withholds every reasoning default but not other defaults",
			body: `{"model":"glm-5.3","reasoning":{"enabled":true}}`,
			defaults: map[string]json.RawMessage{
				"reasoning_effort": json.RawMessage(`"low"`),
				"thinking":         json.RawMessage(`{"type":"disabled"}`),
				"top_p":            json.RawMessage(`0.95`),
			},
			wantApplied: []string{"top_p"},
			want:        map[string]string{"reasoning_effort": "", "thinking": "", "top_p": `0.95`},
		},
		{
			name: "object-valued defaults are applied and reported sorted",
			body: `{"model":"glm-5.3"}`,
			defaults: map[string]json.RawMessage{
				"thinking": json.RawMessage(`{"type":"disabled"}`),
				"top_p":    json.RawMessage(`0.95`),
			},
			wantApplied: []string{"thinking", "top_p"},
			want:        map[string]string{"thinking": `{"type":"disabled"}`, "top_p": `0.95`},
		},
		{
			name: "no defaults only rewrites the model",
			body: `{"model":"glm-5.2","temperature":0.2}`,
			want: map[string]string{"model": `"tg/m"`, "temperature": `0.2`, "reasoning_effort": ""},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, applied, err := RewriteRoutedRequest([]byte(tt.body), "tg/m", tt.defaults)
			if err != nil {
				t.Fatalf("RewriteRoutedRequest: %v", err)
			}
			if strings.Join(applied, ",") != strings.Join(tt.wantApplied, ",") {
				t.Errorf("applied = %v, want %v", applied, tt.wantApplied)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(got, &fields); err != nil {
				t.Fatalf("rewritten body is not a JSON object: %v", err)
			}
			if string(fields["model"]) != `"tg/m"` {
				t.Errorf("model = %s, want the provider-native id", fields["model"])
			}
			for field, want := range tt.want {
				raw, present := fields[field]
				if want == "" {
					if present {
						t.Errorf("field %q must be absent, got %s", field, raw)
					}
					continue
				}
				if string(raw) != want {
					t.Errorf("field %q = %s, want %s", field, raw, want)
				}
			}
		})
	}

	// The catalog's defaults map is shared across requests and must not be mutated.
	if len(low) != 1 {
		t.Errorf("defaults map was mutated: %v", low)
	}
}

func TestRewriteRoutedRequest_RejectsNonObjectBody(t *testing.T) {
	for _, body := range []string{`not json`, `["model"]`, `null`, ``} {
		if _, _, err := RewriteRoutedRequest([]byte(body), "tg/m", nil); err == nil {
			t.Errorf("body %q: expected an error", body)
		}
	}
}
