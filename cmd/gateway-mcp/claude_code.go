package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-majordomo/majordomo-gateway/internal/claudecode"
	"github.com/go-majordomo/majordomo-gateway/internal/pricing"
	"github.com/go-majordomo/majordomo-gateway/internal/sessionclient"
)

// claudeCodeTools answers cost questions about Claude Code sessions straight
// from the transcripts Claude Code keeps on this machine. explain and compare
// need no gateway; import sends usage numbers (never content) to it.
type claudeCodeTools struct {
	c      *client
	apiKey string // gateway API key for import

	once    sync.Once
	cleanup func()
	pricing *pricing.Service
	err     error
}

// calculator lazily builds the local pricing service (bundled catalog plus the
// remote feed), shared by every tool call for the life of the server.
func (t *claudeCodeTools) calculator() (*pricing.Service, error) {
	t.once.Do(func() {
		t.pricing, t.cleanup, t.err = claudecode.LocalPricing()
	})
	return t.pricing, t.err
}

type explainInput struct {
	SessionID string `json:"session_id,omitempty" jsonschema:"Claude Code session id or unique prefix; omit for the current session"`
	MCPConfig string `json:"mcp_config,omitempty" jsonschema:"path to the MCP config (.mcp.json) the session used; when set, MCP tool-definition cost is measured and reported per server (takes ~10s per server)"`
	PerCall   bool   `json:"per_call,omitempty" jsonschema:"list every tool call separately instead of grouping by tool"`
}

type compareInput struct {
	BeforeSessionID string `json:"before_session_id" jsonschema:"the session without the change (id or unique prefix)"`
	AfterSessionID  string `json:"after_session_id" jsonschema:"the session with the change (id or unique prefix)"`
	BeforeMCPConfig string `json:"before_mcp_config,omitempty" jsonschema:"MCP config the before session used, to separate definition cost"`
	AfterMCPConfig  string `json:"after_mcp_config,omitempty" jsonschema:"MCP config the after session used"`
}

type importInput struct {
	SessionID string            `json:"session_id,omitempty" jsonschema:"session id or unique prefix; omit for the current session"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"tags for the run, e.g. {\"variant\": \"baseline\"} or {\"team\": \"growth\"}"`
}

func (t *claudeCodeTools) registerClaudeCodeTools(s *mcp.Server) {

	mcp.AddTool(s, &mcp.Tool{
		Name: "claude_code_explain",
		Description: "Where the cost of a Claude Code session went, per message the user typed: model output, each tool call's result " +
			"(MCP tools are named mcp__<server>__<tool>), MCP tool definitions per server (when mcp_config is given), subagents, " +
			"and the system prompt and conversation history. Costs are API-equivalent: what the tokens would cost at API list prices, " +
			"also for sessions run on a Claude subscription. Omit session_id to explain the current session. Reads local transcripts only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in explainInput) (*mcp.CallToolResult, any, error) {
		r, err := t.report(in.SessionID, in.MCPConfig, in.PerCall)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(r)
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "claude_code_compare",
		Description: "Compare the API-equivalent cost of two Claude Code sessions component by component, e.g. an agent run before and " +
			"after adding a feature, MCP server or prompt change. Returns both totals, the change, and each component (model output, " +
			"each tool's results, MCP definitions per server, each subagent type, system prompt and history) sorted by how much it " +
			"changed. Components don't overlap and sum to each total. Reads local transcripts only.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in compareInput) (*mcp.CallToolResult, any, error) {
		if in.BeforeSessionID == "" || in.AfterSessionID == "" {
			return nil, nil, errors.New("before_session_id and after_session_id are required")
		}
		before, err := t.report(in.BeforeSessionID, in.BeforeMCPConfig, false)
		if err != nil {
			return nil, nil, fmt.Errorf("before: %w", err)
		}
		after, err := t.report(in.AfterSessionID, in.AfterMCPConfig, false)
		if err != nil {
			return nil, nil, fmt.Errorf("after: %w", err)
		}
		return jsonResult(claudecode.Compare(before, after))
	})

	mcp.AddTool(s, &mcp.Tool{
		Name: "claude_code_import",
		Description: "Import a Claude Code session into the Majordomo gateway as an agent run (trace id claude-code:<key-id>:<session-id>), so it " +
			"appears in list_runs / get_run_tree and usage reports. Sends only token counts, model and tool names; never prompts, " +
			"responses or tool content. Re-importing is safe. Omit session_id for the current session. Requires the server to be " +
			"started with --api-key or MAJORDOMO_API_KEY.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in importInput) (*mcp.CallToolResult, any, error) {
		if t.apiKey == "" {
			return nil, nil, errors.New("no gateway API key: start gateway-mcp with --api-key or MAJORDOMO_API_KEY")
		}
		s, err := loadSession(in.SessionID)
		if err != nil {
			return nil, nil, err
		}
		imp := claudecode.BuildImport(s, claudecode.DefaultLabel, in.Metadata)
		if len(imp.Requests) == 0 {
			return nil, nil, errors.New("the session has no model requests yet")
		}
		raw, err := sessionclient.Import(t.c.http, t.c.baseURL, "claude-code", t.apiKey, &imp)
		if err != nil {
			return nil, nil, err
		}
		return jsonResult(raw)
	})
}

func (t *claudeCodeTools) report(sessionID, mcpConfig string, perCall bool) (claudecode.Report, error) {
	s, err := loadSession(sessionID)
	if err != nil {
		return claudecode.Report{}, err
	}
	calc, err := t.calculator()
	if err != nil {
		return claudecode.Report{}, err
	}
	return claudecode.SessionReport(calc, s, mcpConfig, perCall)
}

// loadSession loads a session by id, or the session this server runs in (Claude
// Code passes its id to the MCP servers it starts), or else the most recent one
// started in the working directory.
func loadSession(id string) (*claudecode.Session, error) {
	if id == "" {
		id = os.Getenv(claudecode.CurrentSessionEnv)
	}
	path, err := claudecode.ResolveSession(claudecode.DefaultProjectsDir(), ".", id)
	if err != nil {
		return nil, err
	}
	return claudecode.LoadSession(claudecode.DefaultProjectsDir(), path)
}

func jsonResult(v any) (*mcp.CallToolResult, any, error) {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil, nil, err
	}
	return textResult(raw), nil, nil
}
