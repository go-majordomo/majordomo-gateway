package provider

import (
	"fmt"
	"testing"
)

func TestExtractModel(t *testing.T) {
	tests := []struct {
		name        string
		requestBody []byte
		want        string
	}{
		{
			name:        "OpenAI chat request",
			requestBody: []byte(`{"model": "gpt-4o", "messages": [{"role": "user", "content": "Hi"}]}`),
			want:        "gpt-4o",
		},
		{
			name:        "Anthropic request",
			requestBody: []byte(`{"model": "claude-3-5-sonnet-20241022", "max_tokens": 1024, "messages": []}`),
			want:        "claude-3-5-sonnet-20241022",
		},
		{
			name:        "Gemini request",
			requestBody: []byte(`{"model": "gemini-1.5-pro", "contents": []}`),
			want:        "gemini-1.5-pro",
		},
		{
			name:        "missing model field",
			requestBody: []byte(`{"messages": [{"role": "user", "content": "Hi"}]}`),
			want:        "unknown",
		},
		{
			name:        "empty model field",
			requestBody: []byte(`{"model": "", "messages": []}`),
			want:        "unknown",
		},
		{
			name:        "malformed JSON",
			requestBody: []byte(`{model: invalid`),
			want:        "unknown",
		},
		{
			name:        "empty body",
			requestBody: []byte(``),
			want:        "unknown",
		},
	}

	parsers := []struct {
		name   string
		parser ResponseParser
	}{
		{"OpenAI", &OpenAIParser{}},
		{"Anthropic", &AnthropicParser{}},
		{"Gemini", &GeminiParser{}},
	}

	for _, p := range parsers {
		for _, tt := range tests {
			t.Run(p.name+"/"+tt.name, func(t *testing.T) {
				got := p.parser.ExtractModel(tt.requestBody)
				if got != tt.want {
					t.Errorf("%s.ExtractModel() = %q, want %q", p.name, got, tt.want)
				}
			})
		}
	}
}

func TestProviderDetect(t *testing.T) {
	tests := []struct {
		name         string
		path         string
		headers      map[string]string
		wantProvider Provider
	}{
		{
			name:         "explicit openai header",
			path:         "/anything",
			headers:      map[string]string{"x-majordomo-provider": "openai"},
			wantProvider: ProviderOpenAI,
		},
		{
			name:         "explicit anthropic header",
			path:         "/anything",
			headers:      map[string]string{"x-majordomo-provider": "anthropic"},
			wantProvider: ProviderAnthropic,
		},
		{
			name:         "explicit gemini header case insensitive",
			path:         "/anything",
			headers:      map[string]string{"x-majordomo-provider": "GEMINI"},
			wantProvider: ProviderGemini,
		},
		{
			name:         "explicit gemini-openai header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "gemini-openai"},
			wantProvider: ProviderGeminiOpenAI,
		},
		{
			name:         "explicit fireworks header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "fireworks"},
			wantProvider: ProviderFireworks,
		},
		{
			name:         "explicit together header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "together"},
			wantProvider: ProviderTogether,
		},
		{
			name:         "explicit deepseek header overrides openai path detection",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "deepseek"},
			wantProvider: ProviderDeepSeek,
		},
		{
			name:         "explicit moonshot header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "moonshot"},
			wantProvider: ProviderMoonshot,
		},
		{
			name:         "explicit baseten header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "baseten"},
			wantProvider: ProviderBaseten,
		},
		{
			name:         "explicit nebius header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "nebius"},
			wantProvider: ProviderNebius,
		},
		{
			name:         "explicit deepinfra header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "deepinfra"},
			wantProvider: ProviderDeepInfra,
		},
		{
			name:         "explicit novita header",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "novita"},
			wantProvider: ProviderNovita,
		},
		{
			name:         "explicit fireworks header case insensitive",
			path:         "/v1/chat/completions",
			headers:      map[string]string{"x-majordomo-provider": "FIREWORKS"},
			wantProvider: ProviderFireworks,
		},
		{
			name:         "explicit bedrock-mantle header overrides anthropic path detection",
			path:         "/v1/messages",
			headers:      map[string]string{"x-majordomo-provider": "bedrock-mantle"},
			wantProvider: ProviderBedrockMantle,
		},
		{
			name:         "chat completions path",
			path:         "/v1/chat/completions",
			headers:      map[string]string{},
			wantProvider: ProviderOpenAI,
		},
		{
			name:         "responses API path",
			path:         "/v1/responses",
			headers:      map[string]string{},
			wantProvider: ProviderOpenAI,
		},
		{
			name:         "embeddings path",
			path:         "/v1/embeddings",
			headers:      map[string]string{},
			wantProvider: ProviderOpenAI,
		},
		{
			name:         "anthropic messages path",
			path:         "/v1/messages",
			headers:      map[string]string{},
			wantProvider: ProviderAnthropic,
		},
		{
			name:         "gemini generateContent path",
			path:         "/v1beta/models/gemini-1.5-pro:generateContent",
			headers:      map[string]string{},
			wantProvider: ProviderGemini,
		},
		{
			name:         "gemini streamGenerateContent path",
			path:         "/v1beta/models/gemini-1.5-pro:streamGenerateContent",
			headers:      map[string]string{},
			wantProvider: ProviderGemini,
		},
		{
			name:         "unknown path returns unknown",
			path:         "/some/random/path",
			headers:      map[string]string{},
			wantProvider: ProviderUnknown,
		},
		{
			name:         "explicit header with unknown path",
			path:         "/some/random/path",
			headers:      map[string]string{"x-majordomo-provider": "openai"},
			wantProvider: ProviderOpenAI,
		},
		{
			name:         "explicit header overrides path",
			path:         "/v1/messages",
			headers:      map[string]string{"x-majordomo-provider": "openai"},
			wantProvider: ProviderOpenAI,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			info := Detect(tt.path, tt.headers)
			if info.Provider != tt.wantProvider {
				t.Errorf("Detect() provider = %v, want %v", info.Provider, tt.wantProvider)
			}
		})
	}
}

func TestGetParser(t *testing.T) {
	tests := []struct {
		provider Provider
		wantType string
	}{
		{ProviderOpenAI, "*provider.OpenAIParser"},
		{ProviderAnthropic, "*provider.AnthropicParser"},
		{ProviderGemini, "*provider.GeminiParser"},
		{ProviderGeminiOpenAI, "*provider.OpenAIParser"},     // Gemini OpenAI-compat uses OpenAI parser
		{ProviderAzure, "*provider.OpenAIParser"},            // Azure uses OpenAI parser
		{ProviderFireworks, "*provider.OpenAIParser"},        // Fireworks is OpenAI-compatible
		{ProviderTogether, "*provider.OpenAIParser"},         // Together is OpenAI-compatible
		{ProviderDeepSeek, "*provider.OpenAIParser"},         // DeepSeek is OpenAI-compatible
		{ProviderMoonshot, "*provider.OpenAIParser"},         // Moonshot is OpenAI-compatible
		{ProviderBaseten, "*provider.OpenAIParser"},          // Baseten is OpenAI-compatible
		{ProviderNebius, "*provider.OpenAIParser"},           // Nebius is OpenAI-compatible
		{ProviderDeepInfra, "*provider.OpenAIParser"},        // DeepInfra is OpenAI-compatible
		{ProviderNovita, "*provider.OpenAIParser"},           // Novita is OpenAI-compatible
		{ProviderBedrockMantle, "*provider.AnthropicParser"}, // Bedrock Mantle speaks Anthropic Messages
		{ProviderUnknown, "*provider.OpenAIParser"},          // Unknown defaults to OpenAI
	}

	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			parser := GetParser(tt.provider)
			gotType := fmt.Sprintf("%T", parser)
			if gotType != tt.wantType {
				t.Errorf("GetParser(%v) = %v, want %v", tt.provider, gotType, tt.wantType)
			}
		})
	}
}

func TestNormalizeOpenAIPath(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"chat completions without v1", "/chat/completions", "/v1/chat/completions"},
		{"responses without v1", "/responses", "/v1/responses"},
		{"completions without v1", "/completions", "/v1/completions"},
		{"embeddings without v1", "/embeddings", "/v1/embeddings"},
		{"chat completions with v1 unchanged", "/v1/chat/completions", "/v1/chat/completions"},
		{"responses with v1 unchanged", "/v1/responses", "/v1/responses"},
		{"anthropic messages unchanged", "/v1/messages", "/v1/messages"},
		{"gemini generateContent unchanged", "/v1beta/models/gemini-1.5-pro:generateContent", "/v1beta/models/gemini-1.5-pro:generateContent"},
		{"bedrock converse unchanged", "/model/anthropic.claude-3-sonnet/converse", "/model/anthropic.claude-3-sonnet/converse"},
		{"trailing subpath on responses rewritten", "/responses/abc", "/v1/responses/abc"},
		{"unrelated path containing completions not rewritten", "/foo/completions", "/foo/completions"},
		{"unrelated root path unchanged", "/healthz", "/healthz"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := NormalizeOpenAIPath(tt.in)
			if got != tt.want {
				t.Errorf("NormalizeOpenAIPath(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestUpstreamURLComposition guards the invariant that actually reaches the
// wire: Forward builds the upstream URL as baseURL + path, so the base URL and
// the normalized path together must produce exactly one version segment.
//
// This is the gap that let gemini-openai ship broken — Detect and GetParser
// were both correct for it, and nothing asserted the composed URL.
func TestUpstreamURLComposition(t *testing.T) {
	// The path a client sends to an OpenAI-compatible route, pre-normalization.
	const clientPath = "/chat/completions"

	tests := []struct {
		provider Provider
		want     string
	}{
		{ProviderOpenAI, "https://api.openai.com/v1/chat/completions"},
		{ProviderFireworks, "https://api.fireworks.ai/inference/v1/chat/completions"},
		{ProviderTogether, "https://api.together.xyz/v1/chat/completions"},
		{ProviderDeepSeek, "https://api.deepseek.com/v1/chat/completions"},
		{ProviderMoonshot, "https://api.moonshot.ai/v1/chat/completions"},
		{ProviderBaseten, "https://inference.baseten.co/v1/chat/completions"},
		{ProviderNebius, "https://api.studio.nebius.com/v1/chat/completions"},
		// Path prefix before the version; matches the documented curl exactly.
		{ProviderNovita, "https://api.novita.ai/openai/v1/chat/completions"},
		// Versioned base URLs: the /v1 belongs to the base, not the path.
		{ProviderDeepInfra, "https://api.deepinfra.com/v1/openai/chat/completions"},
		{ProviderGeminiOpenAI, "https://generativelanguage.googleapis.com/v1beta/openai/chat/completions"},
	}

	for _, tt := range tests {
		t.Run(string(tt.provider), func(t *testing.T) {
			info := resolveExplicitProvider(string(tt.provider))

			// Mirrors the handler: normalize on the way in, then strip back off
			// for providers whose base URL already carries a version segment.
			path := NormalizeOpenAIPath(clientPath)
			if BaseURLHasVersionSegment(info.Provider) {
				path = StripOpenAIVersionPrefix(path)
			}

			if got := info.BaseURL + path; got != tt.want {
				t.Errorf("upstream URL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestStripOpenAIVersionPrefix(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"chat completions", "/v1/chat/completions", "/chat/completions"},
		{"completions", "/v1/completions", "/completions"},
		{"embeddings", "/v1/embeddings", "/embeddings"},
		{"responses", "/v1/responses", "/responses"},
		{"trailing segment preserved", "/v1/responses/resp_123", "/responses/resp_123"},
		{"already stripped is left alone", "/chat/completions", "/chat/completions"},
		{"non-OpenAI path untouched", "/v1/messages", "/v1/messages"},
		{"unrelated path untouched", "/anything", "/anything"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := StripOpenAIVersionPrefix(tt.in); got != tt.want {
				t.Errorf("StripOpenAIVersionPrefix(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

// TestNormalizeStripRoundTrip pins the two helpers as exact inverses on every
// OpenAI-compatible route, so adding a suffix to one list cannot silently
// desynchronize them.
func TestNormalizeStripRoundTrip(t *testing.T) {
	for _, suffix := range openAIPathSuffixes {
		t.Run(suffix, func(t *testing.T) {
			if got := StripOpenAIVersionPrefix(NormalizeOpenAIPath(suffix)); got != suffix {
				t.Errorf("round trip of %q = %q, want %q", suffix, got, suffix)
			}
		})
	}
}

// TestIsCredentialProvider covers the allowlist that gates provider-key storage.
// A provider missing from it cannot have a key stored, and the router only ever
// selects endpoints it holds a credential for — so the omission would present as
// a provider that is configured everywhere and silently never routed to.
func TestIsCredentialProvider(t *testing.T) {
	for _, name := range []string{
		"openai", "anthropic", "gemini", "fireworks", "together",
		"deepseek", "moonshot", "baseten", "nebius", "deepinfra", "novita",
	} {
		t.Run(name, func(t *testing.T) {
			if !IsCredentialProvider(name) {
				t.Errorf("IsCredentialProvider(%q) = false, want true", name)
			}
		})
	}

	for _, name := range []string{"majordomo", "bedrock", "not-a-provider", ""} {
		t.Run("rejects/"+name, func(t *testing.T) {
			if IsCredentialProvider(name) {
				t.Errorf("IsCredentialProvider(%q) = true, want false", name)
			}
		})
	}
}
