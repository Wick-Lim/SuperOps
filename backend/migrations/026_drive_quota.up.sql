-- 026: storage quota — make the counter mean one thing, and make it true.
--
-- Migration 025 created workspace_storage and backfilled bytes_used from
-- files.size_bytes. NOTHING has maintained it since: file.Handler.Upload adds
-- bytes and file.Handler.Delete and file.Collect remove them, and none of the
-- three touch the counter. It has therefore already drifted in both directions.
--
-- Enforcement that starts from a wrong number is one support ticket per
-- workspace, so the number is redefined and re-derived here, BEFORE anything
-- refuses an upload on the strength of it.
--
-- INVARIANT I1, established by this migration and depended on by internal/quota:
--
--   workspace_storage.bytes_used
--     = SUM(file_versions.size_bytes) over every version row whose file belongs
--       to the workspace.
--
-- Three consequences worth stating, because each is a product decision that the
-- number encodes:
--
--   * TRASHED FILES STILL COUNT. That is the point of a quota (plan 02 §3), and
--     it is cheaper to say so in the usage endpoint than to explain it to a
--     support desk.
--   * OLD VERSIONS STILL COUNT. Every version is an object in the bucket. The
--     sum is over file_versions rather than over files precisely so that the
--     second version of a file is not free.
--   * A COLLAB FILE COUNTS FOR NOTHING HERE. It has storage_key = '' and
--     size_bytes = 0 and no version row (drive.CreateFile), because its bytes
--     are the CRDT log in Postgres. Those are collab_bytes, recomputed by a job,
--     and 025 split them from bytes_used deliberately: one half is exact at
--     every instant an upload can observe it and the other is eventually
--     consistent, and summing them into one column makes the exact half
--     unauditable.

-- ---------------------------------------------------------------------------
-- Provenance for the eventually-consistent half
-- ---------------------------------------------------------------------------

-- 025 gave bytes_used and collab_bytes one shared updated_at, which defeats the
-- split it had just made: after an upload bumps updated_at, an operator cannot
-- tell whether collab_bytes was computed a minute ago or never. NULL means never.
ALTER TABLE workspace_storage ADD COLUMN collab_bytes_at TIMESTAMPTZ;

-- ---------------------------------------------------------------------------
-- The index the accounting queries need
-- ---------------------------------------------------------------------------

-- Migration 009 dropped idx_files_workspace under "no query in the codebase can
-- use these". There is one now: the usage breakdown and quota.Recompute each
-- scan one workspace's files. Named here so this reads as a deliberate
-- reinstatement rather than as a revert of 009.
CREATE INDEX idx_files_workspace ON files (workspace_id);

-- ---------------------------------------------------------------------------
-- Close I1 for the rows written between 025 and now
-- ---------------------------------------------------------------------------

-- 025 backfilled one version row per files row that existed then.
-- file.Handler.Upload has written none since, so every chat attachment uploaded
-- in between has no version row and would be invisible to the sum below —
-- silently free storage, for exactly the files a busy workspace has most of.
INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by, created_at)
SELECT f.id, f.current_version, f.storage_key, f.size_bytes, f.content_type, f.user_id, f.created_at
  FROM files f
 WHERE f.storage_key <> ''
   AND NOT EXISTS (SELECT 1 FROM file_versions v WHERE v.file_id = f.id)
    ON CONFLICT (file_id, version) DO NOTHING;

-- ---------------------------------------------------------------------------
-- Re-derive the counter, and create the rows 025 could not
-- ---------------------------------------------------------------------------

-- 025 inserted a workspace_storage row for every workspace that existed then.
-- Nothing has created one since — a workspace born after 025 has no row at all,
-- so its first upload would have had nothing to charge.
INSERT INTO workspace_storage (workspace_id, bytes_used, updated_at)
SELECT w.id,
       COALESCE((SELECT SUM(v.size_bytes)
                   FROM file_versions v
                   JOIN files f ON f.id = v.file_id
                  WHERE f.workspace_id = w.id), 0),
       NOW()
  FROM workspaces w
    ON CONFLICT (workspace_id) DO UPDATE
   SET bytes_used = EXCLUDED.bytes_used,
       updated_at = NOW();
