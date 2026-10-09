-- +goose Up
-- Neither limit may be zero or negative: the rate limiter divides by
-- rate_limit_tokens_per_min, and default_max_tokens is the output assumed for
-- a request that sets no cap. Checking in the database refuses a bad value
-- however the row is written, hand-typed SQL included.
ALTER TABLE tenants
    ADD CONSTRAINT tenants_rate_limit_tokens_per_min_positive CHECK (rate_limit_tokens_per_min > 0),
    ADD CONSTRAINT tenants_default_max_tokens_positive CHECK (default_max_tokens > 0);

-- +goose Down
ALTER TABLE tenants
    DROP CONSTRAINT tenants_default_max_tokens_positive,
    DROP CONSTRAINT tenants_rate_limit_tokens_per_min_positive;
