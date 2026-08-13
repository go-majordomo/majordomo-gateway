-- Majordomo Gateway migration 002: encrypted upstream provider credentials.
-- Single-tenant: one credential per provider (UNIQUE provider). Keys are stored
-- AES-256-GCM encrypted with the gateway's ENCRYPTION_KEY; the plaintext is never
-- persisted. Consumed by the provider router to authenticate the chosen upstream.
CREATE TABLE provider_api_keys (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    provider      VARCHAR(100) NOT NULL UNIQUE,
    encrypted_key TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
