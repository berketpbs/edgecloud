-- 013_quotas_used_requests.up.sql
-- Adds per-tenant request-count metering to the quotas table.
-- `used_request_count` reuses the existing `quota_period_start` (added by 009)
-- for month-boundary rollover; no separate period column is needed.
-- `max_requests_per_month` is the per-tenant cap, populated when the tenant's
-- plan changes via service.TenantService.UpdateTenant.
ALTER TABLE quotas
    ADD COLUMN max_requests_per_month INT   NOT NULL DEFAULT 100000,
    ADD COLUMN used_request_count     BIGINT NOT NULL DEFAULT 0;