-- 063: stop a workflow from triggering itself forever.
--
-- THE LOOP IS REACHABLE TODAY AND UNBOUNDED. The catalogue offers
-- `message.created` as a trigger and `post_message` as a step; the poster
-- publishes `superops.<ws>.message.created`; the trigger consumer is bound to
-- that subject. So one workflow — "when a message is posted, post a message" —
-- runs forever, and the idempotency key cannot stop it: the key is the stream
-- sequence, which is genuinely fresh for each genuinely new message. Every
-- iteration is a real event, correctly deduplicated, and there are infinitely
-- many of them.
--
-- Two guards, because they catch different loops.
--
-- DEPTH catches a loop whose provenance we can follow: a run's own output
-- carries where it came from, and a descendant more than a few generations deep
-- is refused. It is exact and it only works through producers that propagate
-- the marker.
--
-- The RATE CAP catches everything else — two workflows triggering each other
-- through a pillar that does not propagate anything, or a producer added later
-- by somebody who never read this file. It is blunt and it does not need the
-- cooperation of the thing in the middle, which is exactly why it is the one
-- that will still be working in a year.

ALTER TABLE workflow_runs
    -- How many workflow generations produced this run. 0 is a human action.
    ADD COLUMN depth INT NOT NULL DEFAULT 0 CHECK (depth >= 0),
    -- The run at the top of the chain, for showing an operator the whole
    -- cascade rather than one link of it. NULL for depth 0.
    ADD COLUMN root_run_id UUID REFERENCES workflow_runs(id) ON DELETE SET NULL;

-- The rate cap's query: how many runs has this workflow started recently.
-- Partial on the recent window would need a mutable predicate, which Postgres
-- forbids, so it is a plain composite and the query supplies the bound.
CREATE INDEX idx_workflow_runs_rate ON workflow_runs (workflow_id, created_at DESC);

-- Show an operator the cascade.
CREATE INDEX idx_workflow_runs_root ON workflow_runs (root_run_id)
    WHERE root_run_id IS NOT NULL;
