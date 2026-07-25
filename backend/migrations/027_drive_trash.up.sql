-- 027: the trash's back half — when a trashed object is permanently removed.
--
-- Migration 025 gave drive_folders and files a trashed_at, and internal/drive
-- sets it. Nothing has ever cleared it, so today the trash is a place things go
-- and never leave: the bytes stay charged to the workspace's quota forever and
-- the object collector deliberately ignores them (internal/file's ListOrphans
-- excludes trashed_at IS NOT NULL precisely so it cannot race this job).
--
-- Two columns, and both exist so the purge is explicable to the person whose
-- file it deleted.

-- When the retention window elapses, in absolute terms rather than as an offset
-- an operator has to recompute. It is stamped at trash time from the deployment's
-- DRIVE_TRASH_RETENTION_DAYS, so changing that setting does NOT retroactively
-- shorten the promise already made about a file somebody trashed last week.
--
-- NULL means "never purge automatically", which is what a deployment with
-- retention disabled writes, and what every row trashed before this migration
-- keeps. A backfill that stamped a date on those would be this migration
-- scheduling the deletion of data nobody was warned about.
ALTER TABLE drive_folders ADD COLUMN purge_after TIMESTAMPTZ;
ALTER TABLE files         ADD COLUMN purge_after TIMESTAMPTZ;

-- The job's working set: trashed, due, oldest first. Partial, so it indexes the
-- handful of rows that are actually pending rather than every file ever stored.
CREATE INDEX idx_drive_folders_purge ON drive_folders (purge_after)
    WHERE trashed_at IS NOT NULL AND purge_after IS NOT NULL;
CREATE INDEX idx_files_purge ON files (purge_after)
    WHERE trashed_at IS NOT NULL AND purge_after IS NOT NULL;

-- The trash LISTING shows only the top of each trashed tree — a folder whose
-- parent is also trashed is not a separate entry, it went with its ancestor.
-- This serves that: find the trashed rows in a workspace, then check the parent.
CREATE INDEX idx_drive_folders_trashed_parent ON drive_folders (workspace_id, parent_id)
    WHERE trashed_at IS NOT NULL;
