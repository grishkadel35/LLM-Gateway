-- +goose Up
-- The budget ledger's Lua scripts do their arithmetic in doubles, which are
-- exact only below 2^53 (about 9.007e15). Capping a budget at 1e15 micros, $1
-- billion, keeps every number they compare below that. And monthly is the only
-- period the gateway enforces: any other value would be a budget that is
-- silently never applied. Checking in the database refuses a bad value however
-- the row is written, hand-typed SQL included.
ALTER TABLE tenants
    ADD CONSTRAINT tenants_budget_micros_range CHECK (budget_micros BETWEEN 0 AND 1000000000000000),
    ADD CONSTRAINT tenants_budget_period_monthly CHECK (budget_period = 'monthly');

-- +goose Down
ALTER TABLE tenants
    DROP CONSTRAINT tenants_budget_period_monthly,
    DROP CONSTRAINT tenants_budget_micros_range;
