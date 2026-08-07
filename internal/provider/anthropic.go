package provider

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strings"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type AnthropicParser struct{}

// anthropicCacheCreation breaks cache-creation tokens down by TTL. Anthropic
// reports this alongside cache_creation_input_tokens (which equals the sum of
// the two fields); the buckets are priced differently (5m writes at 1.25x
// input, 1h writes at 2x input).
type anthropicCacheCreation struct {
	Ephemeral5mInputTokens int `json:"ephemeral_5m_input_tokens"`
	Ephemeral1hInputTokens int `json:"ephemeral_1h_input_tokens"`
}

type anthropicUsage struct {
	InputTokens              int                    `json:"input_tokens"`
	OutputTokens             int                    `json:"output_tokens"`
	CacheReadInputTokens     int                    `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int                    `json:"cache_creation_input_tokens"`
	CacheCreation            anthropicCacheCreation `json:"cache_creation"`
}

// splitCacheCreation returns the 5-minute and 1-hour cache-creation token
// buckets. When Anthropic omits the per-TTL breakdown (older responses, or the
// streaming message_start event), the full cache_creation_input_tokens total is
// attributed to the 5-minute bucket — the API's default cache TTL.
func (u anthropicUsage) splitCacheCreation() (fiveMin, oneHour int) {
	if u.CacheCreation.Ephemeral5mInputTokens == 0 && u.CacheCreation.Ephemeral1hInputTokens == 0 {
		return u.CacheCreationInputTokens, 0
	}
	return u.CacheCreation.Ephemeral5mInputTokens, u.CacheCreation.Ephemeral1hInputTokens
}

type anthropicResponse struct {
	Model string         `json:"model"`
	Usage anthropicUsage `json:"usage"`
}

// SSE event structs for streaming responses
type messageStartEvent struct {
	Message struct {
		Model string         `json:"model"`
		Usage anthropicUsage `json:"usage"`
	} `json:"message"`
}

type messageDeltaEvent struct {
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func (p *AnthropicParser) ParseResponse(body []byte) (*models.UsageMetrics, error) {
	if isSSEResponse(body) {
		return p.parseStreamingResponse(body)
	}

	var resp anthropicResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		return nil, err
	}

	return buildMetrics(&resp), nil
}

func (p *AnthropicParser) ExtractModel(requestBody []byte) string {
	return extractModelFromRequest(requestBody)
}

func (p *AnthropicParser) parseStreamingResponse(body []byte) (*models.UsageMetrics, error) {
	var model string
	var outputTokens int
	var usage anthropicUsage

	scanner := bufio.NewScanner(bytes.NewReader(body))
	var currentEvent string

	for scanner.Scan() {
		line := scanner.Text()

		if strings.HasPrefix(line, "event: ") {
			currentEvent = strings.TrimPrefix(line, "event: ")
			continue
		}

		if !strings.HasPrefix(line, "data: ") {
			continue
		}

		data := strings.TrimPrefix(line, "data: ")

		switch currentEvent {
		case "message_start":
			var event messageStartEvent
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				model = event.Message.Model
				usage = event.Message.Usage
			}
		case "message_delta":
			var event messageDeltaEvent
			if err := json.Unmarshal([]byte(data), &event); err == nil {
				outputTokens = event.Usage.OutputTokens
			}
		}
	}

	fiveMin, oneHour := usage.splitCacheCreation()
	totalInput := usage.InputTokens + usage.CacheReadInputTokens + usage.CacheCreationInputTokens

	return &models.UsageMetrics{
		Provider:              string(ProviderAnthropic),
		Model:                 model,
		InputTokens:           totalInput,
		OutputTokens:          outputTokens,
		CachedTokens:          usage.CacheReadInputTokens,
		CacheCreationTokens:   usage.CacheCreationInputTokens,
		CacheCreation5mTokens: fiveMin,
		CacheCreation1hTokens: oneHour,
	}, nil
}

func isSSEResponse(body []byte) bool {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	return bytes.HasPrefix(trimmed, []byte("event:")) || bytes.HasPrefix(trimmed, []byte("data:"))
}

// buildMetrics creates UsageMetrics from a non-streaming Anthropic response.
func buildMetrics(resp *anthropicResponse) *models.UsageMetrics {
	// Normalize InputTokens to total input (matching OpenAI's convention where
	// prompt_tokens includes cached tokens). Anthropic's input_tokens excludes
	// cache_read and cache_creation tokens, so we add them back.
	totalInput := resp.Usage.InputTokens + resp.Usage.CacheReadInputTokens + resp.Usage.CacheCreationInputTokens
	fiveMin, oneHour := resp.Usage.splitCacheCreation()

	return &models.UsageMetrics{
		Provider:              string(ProviderAnthropic),
		Model:                 resp.Model,
		InputTokens:           totalInput,
		OutputTokens:          resp.Usage.OutputTokens,
		CachedTokens:          resp.Usage.CacheReadInputTokens,
		CacheCreationTokens:   resp.Usage.CacheCreationInputTokens,
		CacheCreation5mTokens: fiveMin,
		CacheCreation1hTokens: oneHour,
	}
}
