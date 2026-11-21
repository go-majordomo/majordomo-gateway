package models

import "strings"

// SessionProvider selects the catalog used for API-equivalent prices.
func SessionProvider(source, model string) string {
	switch {
	case source == "claude-code", strings.HasPrefix(model, "claude"):
		return "anthropic"
	case strings.HasPrefix(model, "gpt-"), source == "codex":
		return "openai"
	case strings.HasPrefix(model, "gemini"):
		return "gemini"
	default:
		return source
	}
}

// SessionSources lists the supported import endpoints.
func SessionSources() []string { return []string{"claude-code", "codex", "cursor"} }
