-- Reverses 027.
--
-- Not lossy in any direction that matters: purge_after is a schedule, not
-- content. Dropping it means nothing is automatically purged, which is exactly
-- the behaviour before this migration — the trashed rows stay trashed and stay
-- charged, and DELETE /drive/trash still empties on demand.

DROP INDEX IF EXISTS idx_drive_folders_trashed_parent;
DROP INDEX IF EXISTS idx_files_purge;
DROP INDEX IF EXISTS idx_drive_folders_purge;

ALTER TABLE files         DROP COLUMN IF EXISTS purge_after;
ALTER TABLE drive_folders DROP COLUMN IF EXISTS purge_after;
