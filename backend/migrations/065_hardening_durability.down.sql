DROP INDEX IF EXISTS idx_domain_event_outbox_unpublished;
DROP TABLE IF EXISTS domain_event_outbox;

ALTER TABLE workflow_effects
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS status;
