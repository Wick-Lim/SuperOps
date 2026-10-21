-- 065: workflow effect state machine and transactional domain event outbox.
--
-- 1. workflow_effects previously recorded a row at ClaimEffect time with no
--    completion status, so a worker killed between ClaimEffect and
--    action.Perform left a row that a subsequent retry treated as 'succeeded'
--    (skipping the step forever). Tracking status ('claimed', 'succeeded',
--    'failed') ensures only 'succeeded' effects are deduplicated on retry while
--    interrupted 'claimed' or 'failed' effects can be re-executed when a stale
--    run is released.
ALTER TABLE workflow_effects
    ADD COLUMN status TEXT NOT NULL DEFAULT 'claimed'
        CHECK (status IN ('claimed', 'succeeded', 'failed')),
    ADD COLUMN updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();

-- Existing rows in workflow_effects were written by completed steps.
UPDATE workflow_effects SET status = 'succeeded' WHERE status = 'claimed';

-- 2. domain_event_outbox records domain events in PostgreSQL so that if
--    JetStream is temporarily unreachable after a database commit, the worker
--    sweeps and republishes unacknowledged events using their deterministic
--    dedupe_id.
CREATE TABLE domain_event_outbox (
    id              BIGSERIAL PRIMARY KEY,
    subject         TEXT NOT NULL,
    dedupe_id       TEXT NOT NULL UNIQUE,
    event_type      TEXT NOT NULL,
    payload         JSONB NOT NULL DEFAULT '{}'::jsonb,
    published_at    TIMESTAMPTZ,
    attempts        INT NOT NULL DEFAULT 0,
    last_error      TEXT,
    next_attempt_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_domain_event_outbox_unpublished
    ON domain_event_outbox (next_attempt_at ASC, id ASC)
 WHERE published_at IS NULL;
