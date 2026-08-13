package proxy

import (
	"testing"

	"github.com/go-majordomo/majordomo-gateway/internal/config"
	"github.com/go-majordomo/majordomo-gateway/internal/provider"
)

func TestShouldConsiderRouting(t *testing.T) {
	tests := []struct {
		name     string
		headers  map[string]string
		provider provider.Provider
		want     bool
	}{
		{
			// Routing is opt-in: the explicit majordomo sentinel on the OpenAI
			// surface is what turns it on.
			name:     "majordomo opt-in on OpenAI surface routes",
			headers:  map[string]string{"x-majordomo-provider": "majordomo"},
			provider: provider.ProviderOpenAI,
			want:     true,
		},
		{
			name:     "majordomo opt-in is case-insensitive",
			headers:  map[string]string{"x-majordomo-provider": "Majordomo"},
			provider: provider.ProviderOpenAI,
			want:     true,
		},
		{
			// No opt-in header ⇒ today's exact pass-through, even when the model
			// would otherwise be routable.
			name:     "clean OpenAI-surface request does not route without opt-in",
			headers:  map[string]string{},
			provider: provider.ProviderOpenAI,
			want:     false,
		},
		{
			// A concrete provider pin always behaves as today (pass-through).
			name:     "explicit provider pin never routes",
			headers:  map[string]string{"x-majordomo-provider": "fireworks"},
			provider: provider.ProviderOpenAI,
			want:     false,
		},
		{
			name:     "majordomo opt-in off the OpenAI surface is out of v1 scope",
			headers:  map[string]string{"x-majordomo-provider": "majordomo"},
			provider: provider.ProviderAnthropic,
			want:     false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := shouldConsiderRouting(tt.headers, tt.provider); got != tt.want {
				t.Errorf("shouldConsiderRouting() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestResolveDataPolicy(t *testing.T) {
	tests := []struct {
		name        string
		defZDR      bool
		defDataColl string
		headers     map[string]string
		wantZDR     bool
		wantNoColl  bool
	}{
		{name: "no default, no header", wantZDR: false, wantNoColl: false},
		{name: "config default requires zdr", defZDR: true, wantZDR: true},
		{name: "config default requires no-collection", defDataColl: "deny", wantNoColl: true},
		{
			name:    "header tightens (adds zdr) on top of no default",
			headers: map[string]string{"x-majordomo-zdr": "true"}, wantZDR: true,
		},
		{
			name:    "header adds no-collection on top of no default",
			headers: map[string]string{"x-majordomo-data-collection": "deny"}, wantNoColl: true,
		},
		{
			// Tighten-only: a header claiming false cannot relax the configured floor.
			name:   "header cannot relax a config zdr default",
			defZDR: true, headers: map[string]string{"x-majordomo-zdr": "false"}, wantZDR: true,
		},
		{
			name:        "header cannot relax a config data-collection default",
			defDataColl: "deny", headers: map[string]string{"x-majordomo-data-collection": "allow"}, wantNoColl: true,
		},
		{
			name:        "union of config default + header",
			defDataColl: "deny",
			headers:     map[string]string{"x-majordomo-zdr": "true"},
			wantZDR:     true, wantNoColl: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := &Handler{config: &config.Config{Routing: config.RoutingConfig{
				DefaultRequireZDR:     tt.defZDR,
				DefaultDataCollection: tt.defDataColl,
			}}}
			got := h.resolveDataPolicy(tt.headers)
			if got.RequireZDR != tt.wantZDR || got.RequireNoDataCollection != tt.wantNoColl {
				t.Errorf("resolveDataPolicy = {zdr:%v noColl:%v}, want {zdr:%v noColl:%v}",
					got.RequireZDR, got.RequireNoDataCollection, tt.wantZDR, tt.wantNoColl)
			}
		})
	}
}
