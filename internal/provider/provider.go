package provider

import (
	"encoding/json"
	"strings"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type Provider string

const (
	ProviderOpenAI          Provider = "openai"
	ProviderAnthropic       Provider = "anthropic"
	ProviderGemini          Provider = "gemini"
	ProviderGeminiOpenAI    Provider = "gemini-openai"    // Gemini via OpenAI-compatible endpoint
	ProviderAnthropicOpenAI Provider = "anthropic-openai" // Anthropic via OpenAI-compatible translation
	ProviderAzure           Provider = "azure"
	ProviderBedrock         Provider = "bedrock"
	ProviderFireworks       Provider = "fireworks"      // Fireworks AI; OpenAI-compatible
	ProviderTogether        Provider = "together"       // Together AI; OpenAI-compatible
	ProviderDeepSeek        Provider = "deepseek"       // DeepSeek; OpenAI-compatible
	ProviderMoonshot        Provider = "moonshot"       // Moonshot AI (Kimi); OpenAI-compatible
	ProviderBaseten         Provider = "baseten"        // Baseten Model APIs; OpenAI-compatible
	ProviderNebius          Provider = "nebius"         // Nebius AI Studio; OpenAI-compatible
	ProviderBedrockMantle   Provider = "bedrock-mantle" // AWS Bedrock Mantle; Anthropic Messages API
	ProviderMajordomo       Provider = "majordomo"      // routing opt-in sentinel; never a real upstream
	ProviderUnknown         Provider = "unknown"
)

// credentialProviders is the set of real upstream providers that authenticate
// with a stored bearer credential (as opposed to AWS-signed Bedrock or the
// routing sentinel). Provider-key management validates against this set so typos
// are rejected before a useless credential is stored.
var credentialProviders = map[Provider]bool{
	ProviderOpenAI:    true,
	ProviderAnthropic: true,
	ProviderGemini:    true,
	ProviderFireworks: true,
	ProviderTogether:  true,
	ProviderDeepSeek:  true,
	ProviderMoonshot:  true,
	ProviderBaseten:   true,
	ProviderNebius:    true,
}

// IsCredentialProvider reports whether name is a known upstream that accepts a
// stored bearer credential for provider routing. name is matched case-insensitively.
func IsCredentialProvider(name string) bool {
	return credentialProviders[Provider(strings.ToLower(name))]
}

type ResponseParser interface {
	ParseResponse(body []byte) (*models.UsageMetrics, error)
	ExtractModel(requestBody []byte) string
}

type ProviderInfo struct {
	Provider Provider
	BaseURL  string
}

func Detect(path string, headers map[string]string) ProviderInfo {
	if explicit, ok := headers["x-majordomo-provider"]; ok {
		// "majordomo" is not a real upstream — it is the opt-in signal for
		// provider routing. Resolve the request surface from the path (routing
		// operates on the OpenAI-compatible surface); the handler's routing layer
		// picks the concrete provider from the model slug.
		if strings.EqualFold(explicit, string(ProviderMajordomo)) {
			return detectFromPath(path)
		}
		return resolveExplicitProvider(explicit)
	}

	return detectFromPath(path)
}

// NormalizeOpenAIPath rewrites OpenAI-compatible paths missing the /v1 prefix
// to their canonical /v1/... form. The OpenAI SDK convention is that base_url
// already includes the version segment (e.g. https://api.openai.com/v1), so
// clients that point base_url at Steward without /v1 send paths like
// /chat/completions or /responses. Normalizing once at the edge lets all
// downstream detection, translation, and upstream forwarding logic assume the
// canonical /v1/... shape. Returns the path unchanged for non-OpenAI-shaped
// routes (Anthropic, Gemini, Bedrock).
func NormalizeOpenAIPath(path string) string {
	for _, suffix := range []string{"/chat/completions", "/completions", "/embeddings", "/responses"} {
		if path == suffix || strings.HasPrefix(path, suffix+"/") {
			return "/v1" + path
		}
	}
	return path
}

func resolveExplicitProvider(name string) ProviderInfo {
	switch Provider(strings.ToLower(name)) {
	case ProviderOpenAI:
		return ProviderInfo{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com"}
	case ProviderAnthropic:
		return ProviderInfo{Provider: ProviderAnthropic, BaseURL: "https://api.anthropic.com"}
	case ProviderGemini:
		return ProviderInfo{Provider: ProviderGemini, BaseURL: "https://generativelanguage.googleapis.com"}
	case ProviderGeminiOpenAI:
		// Gemini's OpenAI-compatible endpoint: https://ai.google.dev/gemini-api/docs/openai
		return ProviderInfo{Provider: ProviderGeminiOpenAI, BaseURL: "https://generativelanguage.googleapis.com/v1beta/openai"}
	case ProviderAnthropicOpenAI:
		// Accepts OpenAI-format requests, translates to Anthropic API format
		return ProviderInfo{Provider: ProviderAnthropicOpenAI, BaseURL: "https://api.anthropic.com"}
	case ProviderAzure:
		return ProviderInfo{Provider: ProviderAzure, BaseURL: ""}
	case ProviderBedrock:
		return ProviderInfo{Provider: ProviderBedrock, BaseURL: ""}
	case ProviderFireworks:
		// BaseURL omits /v1 because NormalizeOpenAIPath adds it to the forwarded path.
		return ProviderInfo{Provider: ProviderFireworks, BaseURL: "https://api.fireworks.ai/inference"}
	case ProviderTogether:
		// BaseURL omits /v1 because NormalizeOpenAIPath adds it to the forwarded path.
		return ProviderInfo{Provider: ProviderTogether, BaseURL: "https://api.together.xyz"}
	case ProviderDeepSeek:
		// BaseURL omits /v1 because NormalizeOpenAIPath adds it to the forwarded path.
		return ProviderInfo{Provider: ProviderDeepSeek, BaseURL: "https://api.deepseek.com"}
	case ProviderMoonshot:
		// OpenAI-compatible endpoint; BaseURL omits /v1 (NormalizeOpenAIPath adds it).
		return ProviderInfo{Provider: ProviderMoonshot, BaseURL: "https://api.moonshot.ai"}
	case ProviderBaseten:
		// Baseten Model APIs (OpenAI-compatible); BaseURL omits /v1.
		return ProviderInfo{Provider: ProviderBaseten, BaseURL: "https://inference.baseten.co"}
	case ProviderNebius:
		// Nebius AI Studio (OpenAI-compatible); BaseURL omits /v1.
		return ProviderInfo{Provider: ProviderNebius, BaseURL: "https://api.studio.nebius.com"}
	case ProviderBedrockMantle:
		// Region-templated at request time; BaseURL is set by the handler after
		// resolving the region from X-Majordomo-Bedrock-Region or Host.
		return ProviderInfo{Provider: ProviderBedrockMantle, BaseURL: ""}
	default:
		return ProviderInfo{Provider: ProviderUnknown, BaseURL: ""}
	}
}

func detectFromPath(path string) ProviderInfo {
	switch {
	case strings.HasPrefix(path, "/v1/chat/completions"),
		strings.HasPrefix(path, "/v1/completions"),
		strings.HasPrefix(path, "/v1/embeddings"),
		strings.HasPrefix(path, "/v1/responses"):
		return ProviderInfo{Provider: ProviderOpenAI, BaseURL: "https://api.openai.com"}

	case strings.HasPrefix(path, "/v1/messages"):
		return ProviderInfo{Provider: ProviderAnthropic, BaseURL: "https://api.anthropic.com"}

	case strings.Contains(path, "generateContent"),
		strings.Contains(path, "streamGenerateContent"):
		return ProviderInfo{Provider: ProviderGemini, BaseURL: "https://generativelanguage.googleapis.com"}

	case strings.HasPrefix(path, "/model/") &&
		(strings.HasSuffix(path, "/converse") || strings.HasSuffix(path, "/converse-stream")):
		// Bedrock base URL is region-specific and resolved from the Host header at request time.
		return ProviderInfo{Provider: ProviderBedrock, BaseURL: ""}

	default:
		return ProviderInfo{Provider: ProviderUnknown, BaseURL: ""}
	}
}

func GetParser(p Provider) ResponseParser {
	switch p {
	case ProviderOpenAI, ProviderAzure, ProviderGeminiOpenAI, ProviderAnthropicOpenAI,
		ProviderFireworks, ProviderTogether, ProviderDeepSeek,
		ProviderMoonshot, ProviderBaseten, ProviderNebius:
		return &OpenAIParser{}
	case ProviderAnthropic, ProviderBedrockMantle:
		return &AnthropicParser{}
	case ProviderGemini:
		return &GeminiParser{}
	case ProviderBedrock:
		return &BedrockParser{}
	default:
		return &OpenAIParser{}
	}
}

func extractModelFromRequest(body []byte) string {
	var req struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(body, &req); err != nil {
		return "unknown"
	}
	if req.Model == "" {
		return "unknown"
	}
	return req.Model
}
