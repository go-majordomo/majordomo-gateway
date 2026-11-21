package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/go-majordomo/majordomo-gateway/internal/agentusage"
	"github.com/go-majordomo/majordomo-gateway/internal/sessionclient"
)

type agentExplainInput struct {
	Measurement string `json:"mcp_measurement,omitempty" jsonschema:"local JSON produced by the matching client mcp-overhead --probe"`
	SessionID   string `json:"session_id" jsonschema:"explicit session id or unique prefix"`
	PerCall     bool   `json:"per_call,omitempty"`
}
type agentCompareInput struct {
	BeforeMeasurement string `json:"before_mcp_measurement,omitempty"`
	AfterMeasurement  string `json:"after_mcp_measurement,omitempty"`
	Before            string `json:"before_session_id"`
	After             string `json:"after_session_id"`
}

type agentImportInput struct {
	SessionID string            `json:"session_id" jsonschema:"explicit session id or unique prefix"`
	Metadata  map[string]string `json:"metadata,omitempty" jsonschema:"tags for the imported run"`
}

func (t *claudeCodeTools) registerAgentTools(server *mcp.Server) {

	for _, source := range []string{"codex"} {
		load := func(ctx context.Context, id string) (*agentusage.Session, error) {
			if strings.TrimSpace(id) == "" {
				return nil, errors.New("session_id is required; specify a session id or unique prefix")
			}
			store, err := agentusage.Open(ctx, source, agentusage.DefaultPath(source))
			if err != nil {
				return nil, err
			}
			entries, err := store.Find(".", id, time.Time{})
			if err != nil {
				return nil, err
			}
			return store.Load(ctx, entries[0])
		}
		report := func(ctx context.Context, id, measurement string, calls bool) (agentusage.Report, error) {
			s, err := load(ctx, id)
			if err != nil {
				return agentusage.Report{}, err
			}
			if measurement != "" {
				if err := s.ApplyMeasurement(measurement); err != nil {
					return agentusage.Report{}, err
				}
			}
			calc, err := t.calculator()
			if err != nil {
				return agentusage.Report{}, err
			}
			r, _, err := agentusage.Analyze(s, calc, calls, source, nil)
			return r, err
		}
		mcp.AddTool(server, &mcp.Tool{Name: source + "_explain", Description: "Explain the API-equivalent cost of a local " + source + " session by turn, tool and recorded MCP definitions. Includes usage basis and unpriced models."}, func(ctx context.Context, _ *mcp.CallToolRequest, in agentExplainInput) (*mcp.CallToolResult, any, error) {
			r, err := report(ctx, in.SessionID, in.Measurement, in.PerCall)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(r)
		})
		mcp.AddTool(server, &mcp.Tool{Name: source + "_compare", Description: "Compare two local " + source + " sessions by component, including tool-result and MCP schema cost."}, func(ctx context.Context, _ *mcp.CallToolRequest, in agentCompareInput) (*mcp.CallToolResult, any, error) {
			if in.Before == "" || in.After == "" {
				return nil, nil, errors.New("both session ids are required")
			}
			a, err := report(ctx, in.Before, in.BeforeMeasurement, false)
			if err != nil {
				return nil, nil, err
			}
			b, err := report(ctx, in.After, in.AfterMeasurement, false)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(agentusage.Compare(a, b))
		})
		mcp.AddTool(server, &mcp.Tool{Name: source + "_import", Description: "Import usage counts, models, tool names and spans for one explicitly selected local " + source + " session into the configured gateway."}, func(ctx context.Context, _ *mcp.CallToolRequest, in agentImportInput) (*mcp.CallToolResult, any, error) {
			if t.apiKey == "" {
				return nil, nil, errors.New("session import needs --api-key or MAJORDOMO_API_KEY")
			}
			s, err := load(ctx, in.SessionID)
			if err != nil {
				return nil, nil, err
			}
			_, imp, err := agentusage.Analyze(s, nil, false, source, in.Metadata)
			if err != nil {
				return nil, nil, err
			}
			raw, err := sessionclient.Import(t.c.http, t.c.baseURL, source, t.apiKey, &imp)
			if err != nil {
				return nil, nil, err
			}
			return jsonResult(raw)
		})
	}
}
