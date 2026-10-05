-- +goose Up
CREATE TABLE tenants (
    id                        TEXT PRIMARY KEY,
    name                      TEXT NOT NULL,
    rate_limit_tokens_per_min BIGINT NOT NULL DEFAULT 100000,
    budget_micros             BIGINT NOT NULL DEFAULT 50000000,  -- $50.00
    budget_period             TEXT NOT NULL DEFAULT 'monthly',
    default_max_tokens        INT NOT NULL DEFAULT 4096,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Keys live apart from tenants so a tenant can hold several at once: rotation
-- issues a new key, moves clients over, then revokes the old one.
CREATE TABLE api_keys (
    id          BIGSERIAL PRIMARY KEY,
    tenant_id   TEXT NOT NULL REFERENCES tenants(id),
    key_hash    BYTEA NOT NULL UNIQUE,   -- SHA-256 of the full key
    key_prefix  TEXT NOT NULL,           -- e.g. "gw_a1b2c3" for display
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    revoked_at  TIMESTAMPTZ              -- NULL while the key is valid
);

CREATE INDEX api_keys_tenant_id_idx ON api_keys (tenant_id);

CREATE TABLE usage_logs (
    id                  BIGSERIAL PRIMARY KEY,
    request_id          TEXT NOT NULL,          -- gateway's X-Request-ID, also in the log line
    provider_request_id TEXT,                   -- provider's own ID, for their support; NULL if none sent
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    api_key_id          BIGINT NOT NULL REFERENCES api_keys(id),
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    endpoint            TEXT NOT NULL,          -- path after the provider prefix, e.g. /v1/chat/completions
    status              INT NOT NULL,           -- HTTP status returned to the client
    input_tokens        INT NOT NULL,
    cached_input_tokens INT NOT NULL DEFAULT 0,
    cache_write_tokens  INT NOT NULL DEFAULT 0, -- Anthropic cache writes, billed above the input rate
    output_tokens       INT NOT NULL,           -- includes reasoning tokens, which are billed as output
    cost_micros         BIGINT,                 -- NULL when the model isn't priced
    cached              BOOLEAN NOT NULL DEFAULT FALSE,
    streamed            BOOLEAN NOT NULL DEFAULT FALSE,
    latency_ms          INT NOT NULL,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Serves both per-tenant usage queries and budget rehydration, which sums a
-- tenant's cost over the current period.
CREATE INDEX usage_logs_tenant_id_created_at_idx ON usage_logs (tenant_id, created_at);

-- +goose Down
DROP TABLE usage_logs;
DROP TABLE api_keys;
DROP TABLE tenants;
