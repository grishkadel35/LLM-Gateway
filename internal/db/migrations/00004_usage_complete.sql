-- +goose Up
-- usage_complete is false when the gateway never saw the provider's final
-- usage report: the client left mid-stream, the provider never answered a
-- request that went out, or a 2xx ended without the report. The row then
-- holds the tokens seen, and as cost_micros what the tenant's budget held for
-- the request, so a rebuild of the month's spend counts it as the ledger did.
-- Older rows aren't backfilled: only dev databases hold them.
ALTER TABLE usage_logs ADD COLUMN usage_complete BOOLEAN NOT NULL DEFAULT true;

-- +goose Down
ALTER TABLE usage_logs DROP COLUMN usage_complete;
