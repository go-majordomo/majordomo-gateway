package agentusage

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type usage struct {
	Input  int `json:"input_tokens"`
	Cached int `json:"cached_input_tokens"`
	Output int `json:"output_tokens"`
	Total  int `json:"total_tokens"`
}
type descriptor struct{ Type, Name, Namespace string }

func serverDefinitions(raw []json.RawMessage) map[string]int {
	defs := map[string]int{}
	for _, b := range raw {
		var d descriptor
		if json.Unmarshal(b, &d) != nil {
			continue
		}
		name := d.Name
		if d.Namespace != "" {
			name = d.Namespace
		}
		p := strings.Split(name, "__")
		if len(p) > 1 && p[0] == "mcp" && p[1] != "" {
			defs[p[1]] += tokens(len(b))
		}
	}
	return defs
}

func resultSize(raw json.RawMessage) int {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return tokens(len(s))
	}
	var blocks []struct{ Text string }
	if json.Unmarshal(raw, &blocks) == nil {
		n := 0
		for _, b := range blocks {
			n += len(b.Text)
		}
		return tokens(n)
	}
	return 0
}

func codexSession(e Entry) (*Session, error) {
	s := &Session{ID: e.ID, Source: "codex"}
	var model, turn, prompt string
	var reset bool
	var cumulative int
	seen := map[string]bool{}
	type call struct {
		name    string
		carrier int
	}
	calls := map[string]call{}
	pending := map[int][]Result{}
	var defs map[string]int
	err := readLines(e.Path, func(line []byte) error {
		var event struct {
			Type      string
			Timestamp time.Time
			Payload   json.RawMessage
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return err
		}
		var b struct {
			Type, Model, Name, Namespace string
			Message                      json.RawMessage
			TurnID                       string `json:"turn_id"`
			CallID                       string `json:"call_id"`
			ResponseID                   string `json:"response_id"`
			Output                       json.RawMessage
			Tools                        []json.RawMessage
			Usage                        usage
			Thread                       usage `json:"thread_token_usage"`
			Info                         *struct {
				Last  usage `json:"last_token_usage"`
				Total usage `json:"total_token_usage"`
			}
			Item struct {
				Type    string
				Content []struct{ Text string }
			}
		}
		if err := json.Unmarshal(event.Payload, &b); err != nil {
			return err
		}
		switch event.Type {
		case "session_meta":
			if s.Started.IsZero() {
				s.Started = event.Timestamp
			}
		case "turn_context":
			model = b.Model
			if b.TurnID != "" {
				turn = b.TurnID
			}
		case "compacted":
			reset = true
		case "response_item":
			switch b.Type {
			case "function_call", "custom_tool_call":
				name := b.Name
				if b.Namespace != "" && !strings.HasPrefix(name, "mcp__") {
					name = b.Namespace + "__" + name
				}
				calls[b.CallID] = call{name, len(s.Requests) + 1}
			case "function_call_output", "custom_tool_call_output":
				if c, ok := calls[b.CallID]; ok {
					idx := max(c.carrier, len(s.Requests))
					pending[idx] = append(pending[idx], Result{b.CallID, c.name, resultSize(b.Output)})
					delete(calls, b.CallID)
				}
			case "additional_tools":
				defs = serverDefinitions(b.Tools)
			case "tool_search_output":
				next := map[string]int{}
				for n, t := range defs {
					next[n] = t
				}
				for n, t := range serverDefinitions(b.Tools) {
					next[n] = t
				}
				defs = next
			}
		case "event_msg":
			switch b.Type {
			case "task_started":
				turn = b.TurnID
				prompt = ""
			case "user_message":
				_ = json.Unmarshal(b.Message, &prompt)
				if b.TurnID == "" && (turn == "" || strings.HasPrefix(turn, "legacy-")) {
					turn = fmt.Sprintf("legacy-%d", len(s.Requests))
				}
			case "item_completed":
				if strings.EqualFold(b.Item.Type, "UserMessage") && len(b.Item.Content) > 0 {
					prompt = b.Item.Content[0].Text
				}
			case "context_compacted":
				reset = true
			}
		}
		var u usage
		var id string
		var total int
		switch {
		case event.Type == "token_usage_record":
			u = b.Usage
			id = b.ResponseID
			total = b.Thread.Total
		case event.Type == "event_msg" && b.Type == "token_count" && b.Info != nil:
			u = b.Info.Last
			total = b.Info.Total.Total
		default:
			return nil
		}
		if total != 0 && total == cumulative || id != "" && seen[id] {
			return nil
		}
		if u.Input == 0 && u.Output == 0 {
			return nil
		}
		if u.Input < 0 || u.Output < 0 || u.Cached < 0 || u.Input < u.Cached {
			return fmt.Errorf("invalid usage counters in session %s", e.ID)
		}
		if id == "" {
			id = fmt.Sprintf("usage-%d-%d", len(s.Requests), total)
		}
		seen[id] = true
		cumulative = total
		if model == "" {
			model = "unknown"
		}
		idx := len(s.Requests)
		s.Requests = append(s.Requests, Request{ClaudeCodeImportRequest: models.ClaudeCodeImportRequest{RequestID: id, Model: model, OccurredAt: event.Timestamp, InputTokens: u.Input - u.Cached, CacheReadTokens: u.Cached, OutputTokens: u.Output}, Turn: turn, Prompt: prompt, Results: pending[idx], Definitions: defs, Reset: reset})
		delete(pending, idx)
		reset = false
		return nil
	})
	return s, err
}
