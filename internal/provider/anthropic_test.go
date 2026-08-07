package provider

import (
	"testing"
)

func TestAnthropicParser_ParseResponse(t *testing.T) {
	parser := &AnthropicParser{}

	tests := []struct {
		name              string
		responseBody      []byte
		wantInput         int
		wantOutput        int
		wantCached        int
		wantCacheCreation int
		wantModel         string
		wantErr           bool
	}{
		{
			name: "claude-3-5-sonnet standard response",
			responseBody: []byte(`{
				"id": "msg_01XFDUDYJgAACzvnptvVoYEL",
				"type": "message",
				"role": "assistant",
				"content": [
					{"type": "text", "text": "Hello! How can I help you today?"}
				],
				"model": "claude-3-5-sonnet-20241022",
				"stop_reason": "end_turn",
				"stop_sequence": null,
				"usage": {
					"input_tokens": 12,
					"output_tokens": 15
				}
			}`),
			wantInput:  12,
			wantOutput: 15,
			wantCached: 0,
			wantModel:  "claude-3-5-sonnet-20241022",
		},
		{
			name: "claude-3-opus with cache_read_input_tokens",
			responseBody: []byte(`{
				"id": "msg_abc123def456",
				"type": "message",
				"role": "assistant",
				"content": [{"type": "text", "text": "Based on the cached context..."}],
				"model": "claude-3-opus-20240229",
				"stop_reason": "end_turn",
				"usage": {
					"input_tokens": 150,
					"output_tokens": 89,
					"cache_read_input_tokens": 2048
				}
			}`),
			wantInput:  2198, // 150 + 2048 (normalized total)
			wantOutput: 89,
			wantCached: 2048,
			wantModel:  "claude-3-opus-20240229",
		},
		{
			name: "cache_creation_input_tokens only",
			responseBody: []byte(`{
				"id": "msg_cache_create",
				"type": "message",
				"role": "assistant",
				"content": [{"type": "text", "text": "Caching established."}],
				"model": "claude-sonnet-4-20250514",
				"stop_reason": "end_turn",
				"usage": {
					"input_tokens": 100,
					"output_tokens": 50,
					"cache_creation_input_tokens": 5000
				}
			}`),
			wantInput:         5100, // 100 + 5000 (normalized total)
			wantOutput:        50,
			wantCached:        0,
			wantCacheCreation: 5000,
			wantModel:         "claude-sonnet-4-20250514",
		},
		{
			name: "both cache_creation and cache_read",
			responseBody: []byte(`{
				"id": "msg_both_cache",
				"type": "message",
				"role": "assistant",
				"content": [{"type": "text", "text": "Mixed cache scenario."}],
				"model": "claude-sonnet-4-20250514",
				"stop_reason": "end_turn",
				"usage": {
					"input_tokens": 200,
					"output_tokens": 75,
					"cache_read_input_tokens": 1024,
					"cache_creation_input_tokens": 3000
				}
			}`),
			wantInput:         4224, // 200 + 1024 + 3000 (normalized total)
			wantOutput:        75,
			wantCached:        1024,
			wantCacheCreation: 3000,
			wantModel:         "claude-sonnet-4-20250514",
		},
		{
			name: "claude-3-haiku minimal response",
			responseBody: []byte(`{
				"id": "msg_xyz",
				"type": "message",
				"role": "assistant",
				"model": "claude-3-haiku-20240307",
				"content": [],
				"usage": {"input_tokens": 5, "output_tokens": 0}
			}`),
			wantInput:  5,
			wantOutput: 0,
			wantCached: 0,
			wantModel:  "claude-3-haiku-20240307",
		},
		{
			name: "tool use response",
			responseBody: []byte(`{
				"id": "msg_01Aq9w938a90dw8q",
				"type": "message",
				"role": "assistant",
				"content": [
					{
						"type": "tool_use",
						"id": "toolu_01A09q90qw90lq917835lgs",
						"name": "get_weather",
						"input": {"location": "San Francisco, CA"}
					}
				],
				"model": "claude-3-5-sonnet-20241022",
				"stop_reason": "tool_use",
				"usage": {
					"input_tokens": 345,
					"output_tokens": 76
				}
			}`),
			wantInput:  345,
			wantOutput: 76,
			wantCached: 0,
			wantModel:  "claude-3-5-sonnet-20241022",
		},
		{
			name:         "malformed JSON",
			responseBody: []byte(`{not valid json`),
			wantErr:      true,
		},
		{
			name:         "empty body",
			responseBody: []byte(``),
			wantErr:      true,
		},
		{
			name:         "streaming SSE response",
			responseBody: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_01\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-opus-4-6\",\"usage\":{\"input_tokens\":100,\"cache_read_input_tokens\":2048,\"cache_creation_input_tokens\":0,\"output_tokens\":0}}}\n\nevent: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\nevent: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hello\"}}\n\nevent: content_block_stop\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":42}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			wantInput:    2148, // 100 + 2048
			wantOutput:   42,
			wantCached:   2048,
			wantModel:    "claude-opus-4-6",
		},
		{
			name:              "streaming SSE with cache creation",
			responseBody:      []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_02\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"claude-sonnet-4-5-20250929\",\"usage\":{\"input_tokens\":200,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":5000,\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":75}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			wantInput:         5200, // 200 + 5000
			wantOutput:        75,
			wantCached:        0,
			wantCacheCreation: 5000,
			wantModel:         "claude-sonnet-4-5-20250929",
		},
		{
			name: "missing usage field returns zero tokens",
			responseBody: []byte(`{
				"id": "msg_abc",
				"model": "claude-3-5-sonnet-20241022"
			}`),
			wantInput:  0,
			wantOutput: 0,
			wantCached: 0,
			wantModel:  "claude-3-5-sonnet-20241022",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parser.ParseResponse(tt.responseBody)
			if tt.wantErr {
				if err == nil {
					t.Error("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.InputTokens != tt.wantInput {
				t.Errorf("InputTokens = %d, want %d", result.InputTokens, tt.wantInput)
			}
			if result.OutputTokens != tt.wantOutput {
				t.Errorf("OutputTokens = %d, want %d", result.OutputTokens, tt.wantOutput)
			}
			if result.CachedTokens != tt.wantCached {
				t.Errorf("CachedTokens = %d, want %d", result.CachedTokens, tt.wantCached)
			}
			if result.CacheCreationTokens != tt.wantCacheCreation {
				t.Errorf("CacheCreationTokens = %d, want %d", result.CacheCreationTokens, tt.wantCacheCreation)
			}
			if result.Model != tt.wantModel {
				t.Errorf("Model = %q, want %q", result.Model, tt.wantModel)
			}
			if result.Provider != "anthropic" {
				t.Errorf("Provider = %q, want %q", result.Provider, "anthropic")
			}
		})
	}
}

func TestAnthropicParser_CacheCreationTTLSplit(t *testing.T) {
	parser := &AnthropicParser{}

	tests := []struct {
		name         string
		responseBody []byte
		wantTotal    int
		want5m       int
		want1h       int
	}{
		{
			name: "non-streaming with per-TTL breakdown",
			responseBody: []byte(`{
				"model": "claude-sonnet-4-5",
				"usage": {
					"input_tokens": 200,
					"output_tokens": 50,
					"cache_read_input_tokens": 1800,
					"cache_creation_input_tokens": 248,
					"cache_creation": {
						"ephemeral_5m_input_tokens": 148,
						"ephemeral_1h_input_tokens": 100
					}
				}
			}`),
			wantTotal: 248,
			want5m:    148,
			want1h:    100,
		},
		{
			// No cache_creation object: the full total is attributed to the
			// 5-minute bucket (Anthropic's default TTL).
			name: "non-streaming without breakdown falls back to 5m",
			responseBody: []byte(`{
				"model": "claude-sonnet-4-5",
				"usage": {
					"input_tokens": 200,
					"output_tokens": 50,
					"cache_creation_input_tokens": 5000
				}
			}`),
			wantTotal: 5000,
			want5m:    5000,
			want1h:    0,
		},
		{
			name:         "streaming message_start with per-TTL breakdown",
			responseBody: []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"claude-sonnet-4-5\",\"usage\":{\"input_tokens\":200,\"cache_read_input_tokens\":0,\"cache_creation_input_tokens\":248,\"cache_creation\":{\"ephemeral_5m_input_tokens\":148,\"ephemeral_1h_input_tokens\":100},\"output_tokens\":0}}}\n\nevent: message_delta\ndata: {\"type\":\"message_delta\",\"usage\":{\"output_tokens\":42}}\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
			wantTotal:    248,
			want5m:       148,
			want1h:       100,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			result, err := parser.ParseResponse(tt.responseBody)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if result.CacheCreationTokens != tt.wantTotal {
				t.Errorf("CacheCreationTokens = %d, want %d", result.CacheCreationTokens, tt.wantTotal)
			}
			if result.CacheCreation5mTokens != tt.want5m {
				t.Errorf("CacheCreation5mTokens = %d, want %d", result.CacheCreation5mTokens, tt.want5m)
			}
			if result.CacheCreation1hTokens != tt.want1h {
				t.Errorf("CacheCreation1hTokens = %d, want %d", result.CacheCreation1hTokens, tt.want1h)
			}
			// The two buckets must always reconcile with the reported total.
			if got := result.CacheCreation5mTokens + result.CacheCreation1hTokens; got != tt.wantTotal {
				t.Errorf("5m+1h = %d, want %d (must equal cache_creation_input_tokens)", got, tt.wantTotal)
			}
		})
	}
}
