-- Reverse of 062. LOSSY IN A WAY THAT MATTERS: dropping owner_id removes the
-- principal every action authorizes against, so any workflow that survives the
-- rollback runs with no authority to check. Disable them all rather than leave
-- that state reachable — re-enabling is one click, an unauthorized post is not
-- recoverable.
UPDATE workflows SET enabled = FALSE;

DROP INDEX IF EXISTS idx_workflows_owner;
ALTER TABLE workflows DROP COLUMN IF EXISTS owner_id;
