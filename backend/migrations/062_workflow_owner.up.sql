-- 062: a workflow runs AS SOMEBODY.
--
-- THIS IS THE DIFFERENCE BETWEEN AN AUTOMATION ENGINE AND AN AUTHORIZATION
-- BYPASS, and migration 060 shipped without it.
--
-- A step receives only the run's workspace. A `post_message` action with
-- nothing but a workspace id has no authority to check against, so it would
-- post into ANY channel in the tenant — including a private one its author
-- cannot read. The rule that makes automation safe is the least surprising one:
--
--     YOU CANNOT AUTOMATE WHAT YOU COULD NOT DO BY HAND.
--
-- Every action therefore checks the OWNER's capability on the target, exactly
-- as if the owner had performed it. A workflow is a saved intention, not a
-- privilege escalation.
--
-- created_by already exists and is not usable for this: it is
-- ON DELETE SET NULL, so offboarding an employee would turn their workflows
-- into unauthorized runners rather than stopping them. owner_id is
-- ON DELETE RESTRICT, which makes "you must reassign or delete their workflows"
-- an explicit step in offboarding instead of a silent capability grant.
--
-- 061 is deliberately skipped: it stays earmarked for the credential vault that
-- arrives with the HTTP node. Taking it here would spend a number reserved for
-- something else.

ALTER TABLE workflows ADD COLUMN owner_id UUID REFERENCES users(id) ON DELETE RESTRICT;

-- Backfill from the author. A workflow whose author is already gone has no
-- owner to attribute it to and cannot be authorized, so it is DELETED rather
-- than left running with nobody accountable — which is the state this migration
-- exists to make impossible. In practice this deletes nothing: the column
-- shipped days ago and every row was written by an integration test.
DELETE FROM workflows WHERE created_by IS NULL;
UPDATE workflows SET owner_id = created_by WHERE owner_id IS NULL;

ALTER TABLE workflows ALTER COLUMN owner_id SET NOT NULL;

-- Offboarding needs to find them: "which workflows would block deleting this
-- user, and who do they belong to now".
CREATE INDEX idx_workflows_owner ON workflows (owner_id);
