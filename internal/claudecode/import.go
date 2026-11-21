package claudecode

import (
	"fmt"
	"strings"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

// DefaultLabel is the top-level step name of imported Claude Code sessions.
const DefaultLabel = "claude-code"

// BuildImport turns a session into an import payload. Each turn becomes the step
// "<label>/turn-<n>" and each subagent "<label>/turn-<n>/agent:<type>-<id>", so
// the run waterfall shows cost per message and per subagent. Only token counts,
// model and tool names, timestamps and ids are included.
func BuildImport(s *Session, label string, metadata map[string]string) models.ClaudeCodeImport {
	if label == "" {
		label = DefaultLabel
	}
	imp := models.ClaudeCodeImport{SessionID: s.ID, Metadata: metadata}
	mainResults := s.Main.carriedResults()
	for _, t := range s.Turns() {
		turnPath := fmt.Sprintf("%s/turn-%d", label, t.Number)
		addThread(&imp, s.Main, mainResults, t.From, t.To, turnPath)
		for _, sub := range t.Subagents {
			addThread(&imp, sub, sub.carriedResults(), 0, len(sub.Requests), turnPath+"/"+AgentStepName(sub))
		}
	}
	return imp
}

// AgentStepName is a subagent's step name, e.g. "agent:researcher-a62041eb".
func AgentStepName(th *Thread) string {
	id := th.AgentID
	if len(id) > 8 {
		id = id[:8]
	}
	return "agent:" + strings.ReplaceAll(th.AgentType, "/", "_") + "-" + id
}

// addThread adds requests [from, to) of a thread, and the tool results they
// carry first, under spanPath.
func addThread(imp *models.ClaudeCodeImport, th *Thread, results []carriedResult, from, to int, spanPath string) {
	for _, r := range th.Requests[from:to] {
		imp.Requests = append(imp.Requests, models.ClaudeCodeImportRequest{
			RequestID:             r.RequestID,
			SpanPath:              spanPath,
			Model:                 r.Model,
			OccurredAt:            r.At,
			InputTokens:           r.InputTokens,
			OutputTokens:          r.OutputTokens,
			CacheReadTokens:       r.CacheReadTokens,
			CacheCreation5mTokens: r.CacheCreation5mTokens,
			CacheCreation1hTokens: r.CacheCreation1hTokens,
		})
	}
	for _, res := range results {
		if res.Gap < from || res.Gap >= to {
			continue
		}
		carrier := th.Requests[res.Gap]
		name := res.ToolName
		if name == "" {
			name = "unknown"
		}
		imp.ToolSpans = append(imp.ToolSpans, models.ClaudeCodeImportToolSpan{
			ToolUseID:    res.ToolUseID,
			SpanPath:     spanPath,
			ToolName:     name,
			Model:        carrier.Model,
			CalledAt:     carrier.At,
			ResultTokens: res.Tokens,

			Charges: res.charges,
		})
	}
}
