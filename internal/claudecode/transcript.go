// Package claudecode reads Claude Code session transcripts (the JSONL files
// under ~/.claude/projects) to reconstruct a session's model requests, tool
// calls and subagents, so their cost can be imported into the gateway or
// explained locally.
//
// Transcripts hold the full conversation. This package only measures them:
// token counts, model names, tool names, timestamps and the length of tool
// results. Callers decide what leaves the machine; the import payload built
// here (see BuildImport) never includes prompt, response or tool content.
//
// The transcript format is Claude Code's internal format, not a published API.
// The fields read here were verified against Claude Code 2.1.x.
package claudecode

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"math"
	"path/filepath"
	"strings"
	"time"
)

// Request is one model API request. Claude Code writes one transcript line per
// content block, each repeating the request's usage; they are merged into one
// Request. Token fields are disjoint, as Anthropic reports them.
type Request struct {
	RequestID             string
	SessionID             string
	Model                 string
	Cwd                   string
	At                    time.Time
	InputTokens           int
	OutputTokens          int
	CacheReadTokens       int
	CacheCreation5mTokens int
	CacheCreation1hTokens int
}

// CacheCreationTokens is the total written to the prompt cache.
func (r Request) CacheCreationTokens() int { return r.CacheCreation5mTokens + r.CacheCreation1hTokens }

// Context is the total input the request carried.
func (r Request) Context() int { return r.InputTokens + r.CacheReadTokens + r.CacheCreationTokens() }

// ToolResult is a tool call's result as it appears in the conversation.
type ToolResult struct {
	ToolUseID string
	ToolName  string
	Chars     int // length of the result text (images count as 0)
	Gap       int // index of the first request that carries it
}

// Prompt is a message the user typed. Its text stays on the local machine.
type Prompt struct {
	Text string
	At   time.Time
	Gap  int // index of the first request that answers it
}

// Thread is one transcript file: a session's main conversation or one subagent.
type Thread struct {
	AgentID         string // "main" for the main conversation
	AgentType       string // "main", or the subagent type (e.g. "general-purpose")
	ParentToolUseID string // subagents: the Agent tool call that launched it
	Requests        []Request
	Results         []ToolResult
	Prompts         []Prompt
	ToolCaller      map[string]int // tool_use id -> index of the request that called it

	otherChars map[int]int  // non-tool user text per gap, for sizing results
	injected   map[int]bool // gaps where Claude Code added its own context
}

// line holds the only transcript fields this package reads.
type line struct {
	Type      string    `json:"type"`
	IsMeta    bool      `json:"isMeta"`
	SessionID string    `json:"sessionId"`
	Cwd       string    `json:"cwd"`
	RequestID string    `json:"requestId"`
	Timestamp time.Time `json:"timestamp"`
	// Attachment is context Claude Code adds on its own (skill and MCP tool
	// listings, hook output, reminders). Only its presence is used.
	Attachment json.RawMessage `json:"attachment"`
	Message    struct {
		ID      string          `json:"id"`
		Model   string          `json:"model"`
		Content json.RawMessage `json:"content"`
		Usage   *struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadTokens     int `json:"cache_read_input_tokens"`
			CacheCreationTokens int `json:"cache_creation_input_tokens"`
			CacheCreation       *struct {
				Ephemeral1h int `json:"ephemeral_1h_input_tokens"`
			} `json:"cache_creation"`
		} `json:"usage"`
	} `json:"message"`
}

type contentBlock struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`          // tool_use
	Name      string          `json:"name"`        // tool_use
	ToolUseID string          `json:"tool_use_id"` // tool_result
	Text      string          `json:"text"`        // text
	Content   json.RawMessage `json:"content"`     // tool_result: string or blocks
}

// parseThread reads one transcript file. A subagent transcript
// (<session>/subagents/agent-<id>.jsonl) also reads its .meta.json sibling for
// the agent type and the tool call that launched it.
func parseThread(files fs.FS, path string) (*Thread, error) {
	th := &Thread{AgentID: "main", AgentType: "main", ToolCaller: map[string]int{}, otherChars: map[int]int{}, injected: map[int]bool{}}
	if filepath.Base(filepath.Dir(path)) == "subagents" {
		th.AgentID = strings.TrimPrefix(strings.TrimSuffix(filepath.Base(path), ".jsonl"), "agent-")
		th.AgentType, th.ParentToolUseID = readSubagentMeta(files, strings.TrimSuffix(path, ".jsonl")+".meta.json")
	}

	byRequest := map[string]int{}
	toolNames := map[string]string{}
	var pendingToolUses []string // tool_use ids seen before their request's usage

	err := scanLines(files, path, func(raw []byte) {
		if !bytes.Contains(raw, []byte(`"usage"`)) && !bytes.Contains(raw, []byte(`"tool_result"`)) &&
			!bytes.Contains(raw, []byte(`"type":"user"`)) && !bytes.Contains(raw, []byte(`"type":"attachment"`)) {
			return
		}
		var l line
		if json.Unmarshal(raw, &l) != nil {
			return
		}
		switch l.Type {
		case "assistant":
			for _, b := range decodeBlocks(l.Message.Content) {
				if b.Type == "tool_use" && b.ID != "" {
					toolNames[b.ID] = b.Name
					pendingToolUses = append(pendingToolUses, b.ID)
				}
			}
			th.addRequest(l, byRequest, &pendingToolUses)
		case "user":
			th.addUserLine(l)
		case "attachment":
			if len(l.Attachment) > 0 {
				th.injected[len(th.Requests)] = true
			}
		}
	})
	if err != nil {
		return nil, err
	}
	for i := range th.Results {
		th.Results[i].ToolName = toolNames[th.Results[i].ToolUseID]
	}
	return th, nil
}

func (th *Thread) addRequest(l line, byRequest map[string]int, pendingToolUses *[]string) {
	u := l.Message.Usage
	if u == nil || l.Message.Model == "" || l.Message.Model == "<synthetic>" {
		return
	}
	key := l.RequestID
	if key == "" {
		key = l.Message.ID
	}
	if key == "" {
		return
	}
	write1h := 0
	if u.CacheCreation != nil {
		write1h = min(u.CacheCreation.Ephemeral1h, u.CacheCreationTokens)
	}
	req := Request{
		RequestID:             key,
		SessionID:             l.SessionID,
		Model:                 l.Message.Model,
		Cwd:                   l.Cwd,
		At:                    l.Timestamp,
		InputTokens:           u.InputTokens,
		OutputTokens:          u.OutputTokens,
		CacheReadTokens:       u.CacheReadTokens,
		CacheCreation5mTokens: u.CacheCreationTokens - write1h,
		CacheCreation1hTokens: write1h,
	}
	idx, seen := byRequest[key]
	switch {
	case !seen:
		idx = len(th.Requests)
		byRequest[key] = idx
		th.Requests = append(th.Requests, req)
	case req.OutputTokens > th.Requests[idx].OutputTokens:
		th.Requests[idx] = req
	}
	for _, id := range *pendingToolUses {
		th.ToolCaller[id] = idx
	}
	*pendingToolUses = nil
}

func (th *Thread) addUserLine(l line) {
	gap := len(th.Requests) // anything seen now is first sent by the next request
	var s string
	if json.Unmarshal(l.Message.Content, &s) == nil {
		th.otherChars[gap] += len(s)
		if !l.IsMeta && !isInjected(s) {
			th.Prompts = append(th.Prompts, Prompt{Text: s, At: l.Timestamp, Gap: gap})
		}
		return
	}
	var typed []string
	for _, b := range decodeBlocks(l.Message.Content) {
		switch b.Type {
		case "tool_result":
			th.Results = append(th.Results, ToolResult{ToolUseID: b.ToolUseID, Chars: resultChars(b.Content), Gap: gap})
		case "text":
			th.otherChars[gap] += len(b.Text)
			typed = append(typed, b.Text)
		}
	}
	if text := strings.Join(typed, " "); text != "" && !l.IsMeta && !isInjected(text) {
		th.Prompts = append(th.Prompts, Prompt{Text: text, At: l.Timestamp, Gap: gap})
	}
}

// RereadsAfter counts the requests after gap that still carry content first
// sent at gap: every later request until the context is compacted (shrinks).
func (th *Thread) RereadsAfter(gap int) int {
	n := 0
	for j := gap + 1; j < len(th.Requests) && th.Requests[j].Context() >= th.Requests[j-1].Context(); j++ {
		n++
	}
	return n
}

// charsPerToken is how many characters of a tool result make one token,
// measured over 503 Claude Code tool results whose size the context growth
// gives exactly (median 2.28; 90% between 1.8 and 3.0).
const charsPerToken = 2.3

// resultFraming is the tokens the API bills around each tool result.
const resultFraming = 28

// estimateTokens sizes a tool result from its characters.
func estimateTokens(chars int) int {
	return int(math.Round(float64(chars)/charsPerToken)) + resultFraming
}

// ResultTokens sizes each tool result (by index into Results). When only tool
// results and typed text arrived between two requests, the context growth is
// their exact size, split by length. When Claude Code also added its own
// context there (skill or MCP tool listings, hook output), the growth is not
// all theirs: each result is then sized from its characters, never more in
// total than the growth, and the rest stays with the context. Sizes include
// the tool-result framing the API bills. Results never sent to the model are
// absent from the map.
func (th *Thread) ResultTokens() map[int]int {
	gapChars := map[int]int{}
	gapEstimate := map[int]int{}
	for _, r := range th.Results {
		gapChars[r.Gap] += r.Chars
		gapEstimate[r.Gap] += estimateTokens(r.Chars)
	}
	out := map[int]int{}
	for i, r := range th.Results {
		g := r.Gap
		if g == 0 || g >= len(th.Requests) {
			continue
		}
		tokens := estimateTokens(r.Chars) // when growth is unusable
		growth := th.Requests[g].Context() - th.Requests[g-1].Context() - th.Requests[g-1].OutputTokens
		switch {
		case growth <= 0:
		case th.injected[g]:
			if est := gapEstimate[g]; est > growth {
				tokens = int(math.Round(float64(growth) * float64(tokens) / float64(est)))
			}
		default:
			if total := gapChars[g] + th.otherChars[g]; total > 0 {
				tokens = int(math.Round(float64(growth) * float64(r.Chars) / float64(total)))
			}
		}
		out[i] = tokens
	}
	return out
}

// isInjected reports whether a user-role message was added by Claude Code itself
// (background task results, reminders, command output) rather than typed.
func isInjected(text string) bool {
	t := strings.TrimSpace(text)
	for _, prefix := range []string{"<task-notification>", "<system-reminder>", "<local-command-stdout>", "<local-command-caveat>"} {
		if strings.HasPrefix(t, prefix) {
			return true
		}
	}
	return false
}

func readSubagentMeta(files fs.FS, path string) (agentType, toolUseID string) {
	var meta struct {
		AgentType string `json:"agentType"`
		ToolUseID string `json:"toolUseId"`
	}
	if b, err := fs.ReadFile(files, path); err == nil {
		_ = json.Unmarshal(b, &meta)
	}
	if meta.AgentType == "" {
		meta.AgentType = "subagent"
	}
	return meta.AgentType, meta.ToolUseID
}

func decodeBlocks(raw json.RawMessage) []contentBlock {
	var blocks []contentBlock
	_ = json.Unmarshal(raw, &blocks)
	return blocks
}

func resultChars(raw json.RawMessage) int {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return len(s)
	}
	n := 0
	for _, b := range decodeBlocks(raw) {
		n += len(b.Text)
	}
	return n
}

// scanLines calls fn for each line of a JSONL file. Lines can be very large.
func scanLines(files fs.FS, path string, fn func([]byte)) error {
	f, err := files.Open(path)
	if err != nil {
		return err
	}
	defer func() { _ = f.Close() }() // Read-only resource; read errors are checked below.
	r := bufio.NewReaderSize(f, 1<<20)
	for {
		b, err := r.ReadBytes('\n')
		if len(b) > 0 {
			fn(b)
		}
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

// resultStarts places results, in transcript order, after the preceding
// response. The transcript omits exact positions for injected context.
func (th *Thread) resultStarts(sizes map[int]int) []int {
	starts := make([]int, len(th.Results))
	totals := map[int]int{}
	for i, n := range sizes {
		totals[th.Results[i].Gap] += n
	}
	offsets := map[int]int{}
	for i, res := range th.Results {
		size, ok := sizes[i]
		if !ok {
			continue
		}
		gap := res.Gap
		prev := th.Requests[gap-1]
		start := min(prev.Context()+prev.OutputTokens, max(0, th.Requests[gap].Context()-totals[gap]))
		starts[i] = start + offsets[gap]
		offsets[gap] += size
	}
	return starts
}
