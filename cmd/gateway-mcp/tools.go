package main

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// usageFilter is the shared input for the usage query tools. All fields are optional;
// the descriptions guide the model on how to compose a query.
type usageFilter struct {
	Preset        string `json:"preset,omitempty" jsonschema:"time window: 7d, 30d, or 90d (default 30d)"`
	Start         string `json:"start,omitempty" jsonschema:"start date YYYY-MM-DD; overrides preset when set"`
	End           string `json:"end,omitempty" jsonschema:"end date YYYY-MM-DD"`
	Provider      string `json:"provider,omitempty" jsonschema:"filter by provider, e.g. openai or anthropic"`
	Model         string `json:"model,omitempty" jsonschema:"filter by exact model name"`
	APIKeyID      string `json:"api_key_id,omitempty" jsonschema:"filter by gateway API key UUID"`
	StatusClass   string `json:"status_class,omitempty" jsonschema:"filter by outcome: error or success"`
	MetadataKey   string `json:"metadata_key,omitempty" jsonschema:"filter by a request metadata key (e.g. feature, team, project)"`
	MetadataValue string `json:"metadata_value,omitempty" jsonschema:"value the metadata_key must equal"`
	Limit         int    `json:"limit,omitempty" jsonschema:"max rows for list tools"`
	Offset        int    `json:"offset,omitempty" jsonschema:"row offset for list tools"`
}

// body converts the filter into the gateway's usage request JSON.
func (f usageFilter) body() map[string]any {
	b := map[string]any{}
	set := func(k, v string) {
		if v != "" {
			b[k] = v
		}
	}
	set("preset", f.Preset)
	set("start", f.Start)
	set("end", f.End)
	set("provider", f.Provider)
	set("model", f.Model)
	set("api_key_id", f.APIKeyID)
	set("status_class", f.StatusClass)
	if f.Limit > 0 {
		b["limit"] = f.Limit
	}
	if f.Offset > 0 {
		b["offset"] = f.Offset
	}
	if f.MetadataKey != "" && f.MetadataValue != "" {
		b["metadata_filters"] = []map[string]string{{"key": f.MetadataKey, "value": f.MetadataValue}}
	}
	return b
}

func registerTools(s *mcp.Server, c *client) {
	// POST usage tools that take the shared filter.
	post := func(name, desc, path string) {
		mcp.AddTool(s, &mcp.Tool{Name: name, Description: desc},
			func(ctx context.Context, _ *mcp.CallToolRequest, in usageFilter) (*mcp.CallToolResult, any, error) {
				raw, err := c.postRaw(path, in.body())
				if err != nil {
					return nil, nil, err
				}
				return textResult(raw), nil, nil
			})
	}

	post("usage_summary", "Total requests, tokens, and cost over a time window.", "/api/v1/usage/summary")
	post("usage_by_model", "Cost and token usage broken down by provider and model.", "/api/v1/usage/models")
	post("usage_by_provider", "Cost and token usage broken down by provider.", "/api/v1/usage/providers")
	post("usage_by_api_key", "Cost and token usage broken down by gateway API key.", "/api/v1/usage/api-keys")
	post("error_summary", "Error-rate totals and a daily error-rate series.", "/api/v1/usage/errors")
	post("recent_errors", "The most recent failed requests (status >= 400).", "/api/v1/usage/errors/recent")
	post("latency_stats", "Response-time percentiles (p50/p95/p99) and a daily series.", "/api/v1/usage/latency")
	post("list_requests", "A page of individual request log rows, newest first.", "/api/v1/usage/requests")
	post("list_runs", "Agent runs (grouped by trace id) with cost rolled up per run, newest first.", "/api/v1/usage/runs")

	// usage_by_metadata groups by a metadata key supplied as a path segment.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "usage_by_metadata",
		Description: "Cost and token usage grouped by the values of one request metadata key (e.g. group by 'feature', 'team', or 'project').",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in metadataGroupInput) (*mcp.CallToolResult, any, error) {
		raw, err := c.postRaw("/api/v1/usage/metadata/"+in.GroupByKey, in.usageFilter.body())
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	})

	// get_run_tree fetches one run's waterfall by trace id.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_run_tree",
		Description: "The full cost waterfall for one agent run: a tree of tool/agent steps and LLM calls with cost rolled up per node. Use a trace id from list_runs.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in traceInput) (*mcp.CallToolResult, any, error) {
		raw, err := c.getRaw("/api/v1/usage/runs/" + in.TraceID)
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	})

	// get_request_body fetches the archived request/response bodies for one request.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_request_body",
		Description: "Fetch the archived request and response bodies for a single request (only available when body archival to S3/GCS is enabled). Use a request id from list_requests.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestInput) (*mcp.CallToolResult, any, error) {
		raw, err := c.getRaw("/api/v1/usage/requests/" + in.RequestID + "/body")
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	})

	// list_metadata_keys shows discovered metadata keys and their cardinality.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "list_metadata_keys",
		Description: "List the request-metadata keys the gateway has discovered, with approximate cardinality and whether each is indexed (queryable). Use this to see which dimensions (e.g. feature, team, project) can be grouped/filtered; high-cardinality keys like user_id are typically left un-indexed.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, any, error) {
		raw, err := c.getRaw("/api/v1/metadata")
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	})

	// get_request fetches the full detail (including bodies) for one request.
	mcp.AddTool(s, &mcp.Tool{
		Name:        "get_request",
		Description: "Full detail for a single logged request, including request/response bodies when body logging is enabled. Use a request id from list_requests.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in requestInput) (*mcp.CallToolResult, any, error) {
		raw, err := c.getRaw("/api/v1/usage/requests/" + in.RequestID)
		if err != nil {
			return nil, nil, err
		}
		return textResult(raw), nil, nil
	})
}

type metadataGroupInput struct {
	usageFilter
	GroupByKey string `json:"group_by_key" jsonschema:"the metadata key whose values to group by, e.g. feature, team, or project"`
}

type traceInput struct {
	TraceID string `json:"trace_id" jsonschema:"the run's trace id"`
}

type requestInput struct {
	RequestID string `json:"request_id" jsonschema:"the request UUID"`
}

// textResult wraps a raw JSON payload as MCP text content.
func textResult(raw json.RawMessage) *mcp.CallToolResult {
	return &mcp.CallToolResult{
		Content: []mcp.Content{&mcp.TextContent{Text: string(raw)}},
	}
}
