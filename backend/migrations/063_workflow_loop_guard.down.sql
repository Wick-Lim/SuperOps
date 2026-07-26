-- Reverse of 063. LOSSY IN A WAY THAT MATTERS: without depth, a workflow that
-- triggers itself has no bound at all, and the loop this migration exists to
-- stop becomes reachable again. Disable every workflow rather than leave that
-- state running unattended.
UPDATE workflows SET enabled = FALSE;

DROP INDEX IF EXISTS idx_workflow_runs_root;
DROP INDEX IF EXISTS idx_workflow_runs_rate;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS root_run_id;
ALTER TABLE workflow_runs DROP COLUMN IF EXISTS depth;
