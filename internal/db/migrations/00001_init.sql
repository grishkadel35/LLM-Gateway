-- +goose Up
CREATE TABLE tenants (
    id                        TEXT PRIMARY KEY,
    name                      TEXT NOT NULL,
    key_hash                  BYTEA NOT NULL UNIQUE,   -- SHA-256 of the full key
    key_prefix                TEXT NOT NULL,           -- e.g. "gw_a1b2c3" for display
    rate_limit_tokens_per_min BIGINT NOT NULL DEFAULT 100000,
    budget_micros             BIGINT NOT NULL DEFAULT 50000000,  -- $50.00
    budget_period             TEXT NOT NULL DEFAULT 'monthly',
    default_max_tokens        INT NOT NULL DEFAULT 4096,
    created_at                TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE usage_logs (
    id                  BIGSERIAL PRIMARY KEY,
    tenant_id           TEXT NOT NULL REFERENCES tenants(id),
    provider            TEXT NOT NULL,
    model               TEXT NOT NULL,
    endpoint            TEXT NOT NULL,          -- path after the provider prefix, e.g. /v1/chat/completions
    status              INT NOT NULL,           -- HTTP status returned to the client
    input_tokens        INT NOT NULL,
    cached_input_tokens INT NOT NULL DEFAULT 0,
    output_tokens       INT NOT NULL,
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
DROP TABLE tenants;
