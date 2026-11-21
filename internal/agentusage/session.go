// Package agentusage reconstructs cost from local Codex and Cursor history.
package agentusage

import (
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
	"github.com/go-majordomo/majordomo-gateway/internal/models"
)

type Session struct {
	ID, Source    string
	Requests      []Request
	Children      []*Session
	Started       time.Time
	EstimatedTime bool
	Estimated     bool
}
type Request struct {
	models.ClaudeCodeImportRequest
	Turn        string
	Prompt      string
	Reset       bool
	Results     []Result
	Definitions map[string]int
}
type Result struct {
	ID, Name string
	Tokens   int
}

func tokens(bytes int) int { return (bytes + 3) / 4 }

type Report struct {
	claudecode.Report
	EstimatedTime bool `json:"estimated_time,omitempty"`
	Input         int  `json:"input_tokens"`
	Cached        int  `json:"cached_input_tokens"`
	OutputTokens  int  `json:"output_tokens"`
	Estimated     bool `json:"estimated_usage"`
}

// Analyze translates client records into the common session model. Reporting and
// import consume the same request, result placement and model pricing rules.
func Analyze(s *Session, calc claudecode.Calculator, perCall bool, label string, meta map[string]string) (Report, models.ClaudeCodeImport, error) {
	if len(s.Requests) == 0 {
		return Report{}, models.ClaudeCodeImport{}, errors.New("session contains no model usage")
	}
	var report Report
	main := s.thread(&report, false)
	session := &claudecode.Session{ID: s.ID, Main: main}
	main.ToolCaller = map[string]int{}
	turns := session.Turns()
	var addChildren func(*Session, int, int, string)
	addChildren = func(parent *Session, owner, turn int, parentPath string) {
		for _, child := range parent.Children {
			if len(child.Requests) == 0 {
				continue
			}
			if parent == s {
				owner = max(0, sort.Search(len(main.Requests), func(i int) bool { return main.Requests[i].At.After(child.Started) })-1)
			}
			if parent == s {
				turn = sort.Search(len(turns), func(i int) bool { return turns[i].To > owner }) + 1
			}
			sub := child.thread(&report, true)
			sub.ParentToolUseID = child.ID
			sub.ParentPath = parentPath
			main.ToolCaller[child.ID] = owner
			session.Subagents = append(session.Subagents, sub)
			addChildren(child, owner, turn, fmt.Sprintf("%s%s/turn-%d/", parentPath, sub.StepName, turn))
		}
	}
	addChildren(s, 0, 0, "")
	if label == "" {
		label = s.Source
	}
	payload := claudecode.BuildImport(session, label, meta)
	if calc != nil {
		report.Report = claudecode.BuildReport(calc, session, claudecode.ReportOptions{PerCall: perCall})
	}
	if report.Estimated || report.EstimatedTime {
		tags := map[string]string{}
		for k, v := range meta {
			tags[k] = v
		}
		if report.Estimated {
			tags["usage_basis"] = "estimated"
			report.Note += " Missing counters are estimated from visible text at four bytes per token, with no assumed cache discount."
		}
		if report.EstimatedTime {
			tags["time_basis"] = "session_start"
			report.Note += " Missing message timestamps use the session start."
		}
		payload.Metadata = tags
	}
	return report, payload, nil
}

func (s *Session) thread(report *Report, child bool) *claudecode.Thread {
	th := &claudecode.Thread{AgentID: s.ID, AgentType: s.Source, StepName: "agent:" + s.ID}
	for i, r := range s.Requests {
		id := r.RequestID
		if child {
			id = s.ID + "/" + id
		}
		req := claudecode.Request{RequestID: id, Model: r.Model, Provider: models.SessionProvider(s.Source, r.Model), At: r.OccurredAt,
			InputTokens: r.InputTokens, OutputTokens: r.OutputTokens, CacheReadTokens: r.CacheReadTokens,
			CacheCreation5mTokens: r.CacheCreation5mTokens, CacheCreation1hTokens: r.CacheCreation1hTokens,
			Reset: r.Reset, Definitions: r.Definitions}
		th.Requests = append(th.Requests, req)
		if i == 0 || r.Turn != s.Requests[i-1].Turn {
			th.Prompts = append(th.Prompts, claudecode.Prompt{Text: r.Prompt, At: r.OccurredAt, Gap: i})
		}
		tail := req.Context()
		for _, res := range r.Results {
			tail = max(0, tail-res.Tokens)
		}
		for _, res := range r.Results {
			th.Results = append(th.Results, claudecode.ToolResult{ToolUseID: s.ID + "/" + res.ID, ToolName: res.Name, Gap: i})
			th.Placements = append(th.Placements, claudecode.Placement{Start: tail, Tokens: res.Tokens})
			tail = min(req.Context(), tail+res.Tokens)
		}
		report.Input += req.Context()
		report.Cached += req.CacheReadTokens
		report.OutputTokens += req.OutputTokens
	}
	report.Estimated = report.Estimated || s.Estimated
	report.EstimatedTime = report.EstimatedTime || s.EstimatedTime
	return th
}

func Compare(before, after Report) claudecode.Comparison {
	c := claudecode.Compare(before.Report, after.Report)
	if before.Estimated || after.Estimated {
		c.Note += " Includes reconstructed Cursor usage."
	}
	if len(before.Unpriced)+len(after.Unpriced) > 0 {
		c.Note += fmt.Sprintf(" Unpriced models (before %v, after %v) count as zero.", before.Unpriced, after.Unpriced)
	}
	return c
}
