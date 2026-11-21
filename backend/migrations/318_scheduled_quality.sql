-- Existing plans remain ordinary connectivity probes. Only explicit administrator
-- edits advance the revision; periodic last/next-run updates leave it unchanged.
ALTER TABLE scheduled_test_plans
    ADD COLUMN IF NOT EXISTS quality_enabled boolean NOT NULL DEFAULT false,
    ADD COLUMN IF NOT EXISTS quality_revision bigint NOT NULL DEFAULT 1;
ALTER TABLE scheduled_test_results ADD COLUMN IF NOT EXISTS quality jsonb;
