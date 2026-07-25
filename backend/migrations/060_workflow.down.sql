-- Reverse of 060. Lossy in one way worth naming: dropping workflow_runs and
-- workflow_effects destroys the record of what the automation actually did,
-- which is the only place that history exists. The side effects themselves —
-- messages posted, notifications sent — survive, so a rollback leaves results
-- with no explanation.

DROP TRIGGER IF EXISTS trg_workflows_updated_at ON workflows;

DROP TABLE IF EXISTS workflow_trigger_rejections;
DROP TABLE IF EXISTS workflow_effects;
DROP TABLE IF EXISTS workflow_step_runs;
DROP TABLE IF EXISTS workflow_runs;
DROP TABLE IF EXISTS workflow_triggers;
DROP TABLE IF EXISTS workflow_versions;
DROP TABLE IF EXISTS workflows;
