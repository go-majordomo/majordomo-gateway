-- Majordomo Gateway migration 003: provider-routing decision trace.
-- When the gateway routes a virtual model slug to a chosen provider endpoint, it
-- records which provider won, why, and the canonical slug the client originally
-- requested. Columns are additive and nullable so existing rows are unaffected
-- and non-routed requests simply leave them NULL.
ALTER TABLE llm_requests ADD COLUMN IF NOT EXISTS routed_provider        TEXT;
ALTER TABLE llm_requests ADD COLUMN IF NOT EXISTS routing_reason         TEXT;
ALTER TABLE llm_requests ADD COLUMN IF NOT EXISTS routing_original_model TEXT;

-- Serves the router's health aggregation (recent per-provider/model outcomes)
-- over a bounded time window.
CREATE INDEX IF NOT EXISTS idx_llm_requests_provider_model_requested_at
    ON llm_requests (provider, model, requested_at);
