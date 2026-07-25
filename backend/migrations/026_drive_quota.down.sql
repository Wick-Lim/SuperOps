-- Reverses 026.
--
-- LOSSY IN ONE DIRECTION, AND DELIBERATELY SO.
--
-- The file_versions rows the up-migration created are NOT deleted, and neither
-- is the bytes_used it re-derived.
--
-- Deleting those version rows would be the dangerous kind of rollback: the
-- bucket sweep treats an object with no row naming it as garbage
-- (internal/file/repository.go, StorageKeysPresent arm 3), so removing them
-- would make the next object_gc run delete customer data in order to undo a
-- migration. A rollback must not be able to do that.
--
-- bytes_used is left at the true value because the value it replaced was wrong —
-- 025's number had drifted in both directions before this migration ran, and
-- restoring a wrong number is not a restoration.
--
-- What comes back out is only what 026 added and nothing depends on.

DROP INDEX IF EXISTS idx_files_workspace;

ALTER TABLE workspace_storage DROP COLUMN IF EXISTS collab_bytes_at;
