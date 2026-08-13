-- Reverse migration 003: drop the provider-routing decision-trace columns and index.
DROP INDEX IF EXISTS idx_llm_requests_provider_model_requested_at;

ALTER TABLE llm_requests DROP COLUMN IF EXISTS routing_original_model;
ALTER TABLE llm_requests DROP COLUMN IF EXISTS routing_reason;
ALTER TABLE llm_requests DROP COLUMN IF EXISTS routed_provider;
