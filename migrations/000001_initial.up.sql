-- Majordomo Gateway: initial schema.
-- A self-hosted, single-tenant LLM gateway. Two tables: locally-minted API keys
-- and the request log. No orgs, no Butler sync, no cloud body storage.

-- API keys are minted locally by the CLI/admin API. Only the SHA-256 hash of the
-- key is stored; the plaintext ("mdm_sk_...") is shown once at creation.
CREATE TABLE api_keys (
    id                        UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    key_hash                  VARCHAR(64) NOT NULL UNIQUE,
    name                      VARCHAR(255) NOT NULL,
    description               TEXT,
    is_active                 BOOLEAN NOT NULL DEFAULT true,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    revoked_at                TIMESTAMPTZ,
    last_used_at              TIMESTAMPTZ,
    request_count             BIGINT NOT NULL DEFAULT 0,
    updated_at                TIMESTAMPTZ NOT NULL DEFAULT now(),
    deprecated_model_behavior VARCHAR(20) NOT NULL DEFAULT 'passthrough'
);

CREATE INDEX idx_api_keys_hash ON api_keys (key_hash) WHERE is_active = true;

-- One row per proxied request, with token counts, computed cost, request metadata,
-- and agent-run trace/span identity.
CREATE TABLE llm_requests (
    id                          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    majordomo_api_key_id        UUID REFERENCES api_keys(id) ON DELETE SET NULL,
    provider_api_key_hash       VARCHAR(64),
    provider_api_key_alias      VARCHAR(255),
    provider                    VARCHAR(100) NOT NULL,
    model                       VARCHAR(100) NOT NULL,
    request_path                TEXT NOT NULL,
    request_method              TEXT NOT NULL,
    requested_at                TIMESTAMPTZ NOT NULL,
    responded_at                TIMESTAMPTZ NOT NULL,
    response_time_ms            INT NOT NULL,
    input_tokens                INT NOT NULL,
    output_tokens               INT NOT NULL,
    cached_tokens               INT NOT NULL DEFAULT 0,
    cache_creation_tokens       INT NOT NULL DEFAULT 0,
    cache_creation_5m_tokens    INT NOT NULL DEFAULT 0,
    cache_creation_1h_tokens    INT NOT NULL DEFAULT 0,
    input_cost                  NUMERIC(12, 8) NOT NULL,
    cached_cost                 NUMERIC(12, 8) NOT NULL DEFAULT 0,
    cache_creation_cost         NUMERIC(12, 8) NOT NULL DEFAULT 0,
    output_cost                 NUMERIC(12, 8) NOT NULL,
    total_cost                  NUMERIC(12, 8) NOT NULL,
    status_code                 INT NOT NULL,
    error_message               TEXT,
    raw_metadata                JSONB,
    indexed_metadata            JSONB DEFAULT '{}',
    body_s3_key                 TEXT,
    model_alias_found           BOOLEAN NOT NULL DEFAULT true,
    deprecated_model_redirected BOOLEAN NOT NULL DEFAULT false,
    original_model              VARCHAR(100),
    trace_id                    TEXT,
    span_path                   TEXT,
    span_name                   TEXT,
    span_id                     UUID,
    parent_span_id              UUID,
    created_at                  TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Usage listing/aggregation by key over a time range.
CREATE INDEX idx_llm_requests_key_time ON llm_requests (majordomo_api_key_id, requested_at DESC);
-- Slice usage by any metadata key/value (feature/team/project, etc.).
CREATE INDEX idx_llm_requests_indexed_metadata_gin ON llm_requests USING GIN (indexed_metadata);
-- Group a run's calls when assembling its waterfall; partial to stay small since
-- only requests carrying a trace id participate.
CREATE INDEX idx_llm_requests_trace_id ON llm_requests (trace_id) WHERE trace_id IS NOT NULL;

-- Discovered request-metadata keys, per API key. Keys are recorded automatically as
-- they are seen; hll_state / approx_cardinality track the distinct-value count so
-- high-dimension keys can be spotted. Only keys with is_active = true are copied into
-- llm_requests.indexed_metadata (and thus become queryable), keeping the GIN index
-- bounded regardless of what callers send.
CREATE TABLE llm_requests_metadata_keys (
    majordomo_api_key_id UUID NOT NULL REFERENCES api_keys(id) ON DELETE CASCADE,
    key_name             VARCHAR(255) NOT NULL,
    display_name         VARCHAR(255),
    key_type             VARCHAR(50) DEFAULT 'string',
    is_required          BOOLEAN DEFAULT false,
    is_active            BOOLEAN NOT NULL DEFAULT false,
    activated_at         TIMESTAMPTZ,
    request_count        BIGINT NOT NULL DEFAULT 0,
    last_seen_at         TIMESTAMPTZ,
    hll_state            BYTEA,
    approx_cardinality   INT NOT NULL DEFAULT 0,
    hll_updated_at       TIMESTAMPTZ,
    created_at           TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (majordomo_api_key_id, key_name)
);

CREATE INDEX idx_llm_requests_metadata_keys_active
    ON llm_requests_metadata_keys (majordomo_api_key_id) WHERE is_active = true;
