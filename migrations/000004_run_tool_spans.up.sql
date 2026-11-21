-- Majordomo Gateway migration 004: tool spans (agent-run observability Tier 2).
-- One row per tool call in an agent run, so the run waterfall can show where a
-- run's cost went at tool level. Rows are written by usage imports (e.g. Claude
-- Code sessions via `gateway-cli claude-code import`) and hold the tool name and
-- token counts only, never tool inputs or results.
--
-- attributed_cost is the part of the parent step's LLM cost spent carrying the
-- tool's result (first written into the context, then re-read from the prompt
-- cache). It is already included in llm_requests.total_cost, so it must never be
-- added to a run's total.
CREATE TABLE run_tool_spans (
    id                   UUID PRIMARY KEY,
    majordomo_api_key_id UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    trace_id             TEXT NOT NULL,
    span_path            TEXT NOT NULL,
    parent_span_id       UUID NOT NULL,
    tool_name            TEXT NOT NULL,
    mcp_server           TEXT,
    provider             VARCHAR(100) NOT NULL,
    model                VARCHAR(100) NOT NULL,
    called_at            TIMESTAMPTZ NOT NULL,
    result_tokens        INT NOT NULL DEFAULT 0,

    reread_count         INT NOT NULL DEFAULT 0,
    attributed_cost      NUMERIC(12, 8) NOT NULL DEFAULT 0,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX idx_run_tool_spans_trace_id ON run_tool_spans (trace_id);
