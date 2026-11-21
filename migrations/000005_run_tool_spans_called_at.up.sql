-- Bound the per-server usage scan to MCP calls within the requested time range.
CREATE INDEX idx_run_tool_spans_called_at ON run_tool_spans (called_at)
    WHERE mcp_server IS NOT NULL;
