-- Reverses 025.
--
-- LOSSY, in one direction that matters: dropping drive_folders and the Drive
-- columns on files discards where every Drive file lived. The objects survive
-- in the bucket and the files rows survive, but they revert to being rows with
-- no folder and no message — which is exactly what the object GC collects 24
-- hours later. An operator rolling this back with Drive data in it must stop
-- the worker first, or take the data loss knowingly.
--
-- The views are restored to their migration-016 text, not merely dropped: they
-- are the definition of derived ACL state, and leaving 021's database without
-- them would make internal/authz's Rebuild and the drift verifier fail rather
-- than degrade.

DROP VIEW acl_key_expected;
DROP VIEW acl_object_expected;

CREATE VIEW acl_object_expected AS
    SELECT 'workspace'::TEXT AS object_type,
           w.id              AS object_id,
           w.id              AS workspace_id,
           '/workspace:' || w.id::TEXT || '/' AS path
      FROM workspaces w
UNION ALL
    SELECT 'channel'::TEXT, c.id, c.workspace_id,
           '/workspace:' || c.workspace_id::TEXT || '/channel:' || c.id::TEXT || '/'
      FROM channels c
UNION ALL
    SELECT 'file'::TEXT, f.id, f.workspace_id,
           CASE WHEN ch.id IS NULL
                THEN '/workspace:' || f.workspace_id::TEXT || '/file:' || f.id::TEXT || '/'
                ELSE '/workspace:' || f.workspace_id::TEXT || '/channel:' || ch.id::TEXT
                     || '/file:' || f.id::TEXT || '/'
           END
      FROM files f
      LEFT JOIN messages m  ON m.id = f.message_id
      LEFT JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

CREATE VIEW acl_key_expected AS
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT AS key
      FROM acl_object o
     WHERE o.object_type = 'workspace'
UNION
    SELECT o.object_type, o.object_id, 'u-' || cm.user_id::TEXT
      FROM acl_object o
      JOIN channel_members cm ON cm.channel_id = o.object_id
     WHERE o.object_type = 'channel'
UNION
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT
      FROM acl_object o
      JOIN channels c ON c.id = o.object_id
     WHERE o.object_type = 'channel' AND c.type = 'public'
UNION
    SELECT o.object_type, o.object_id, acl_container_key(acl_parent_segment(o.path))
      FROM acl_object o
     WHERE acl_container_key(acl_parent_segment(o.path)) IS NOT NULL
UNION
    SELECT o.object_type, o.object_id, 'u-' || f.user_id::TEXT
      FROM acl_object o
      JOIN files f ON f.id = o.object_id
     WHERE o.object_type = 'file'
       AND split_part(coalesce(acl_parent_segment(o.path), ''), ':', 1) = 'workspace'
UNION
    SELECT d.object_type, d.object_id,
           CASE g.subject_type WHEN 'user' THEN 'u-' WHEN 'group' THEN 'g-' END
               || g.subject_id::TEXT
      FROM acl_grant g
      JOIN acl_object go ON go.object_type = g.object_type AND go.object_id = g.object_id
      JOIN acl_object d  ON d.path LIKE go.path || '%'
     WHERE g.subject_type IN ('user', 'group');

-- acl_object rows for folders name a type whose table is about to disappear.
-- Dropping them takes their acl_grant and acl_key rows by cascade.
DELETE FROM acl_object WHERE object_type = 'folder';

-- Any remaining workspace-subject grant has to go before the CHECK is narrowed
-- again, or the ALTER fails on rows it cannot describe. There should be none
-- left — the only ones this migration created were on folders — but a grant
-- written by a later feature would otherwise turn a rollback into an outage.
DELETE FROM acl_grant WHERE subject_type = 'workspace';

ALTER TABLE acl_grant
    DROP CONSTRAINT acl_grant_subject_type_valid,
    ADD  CONSTRAINT acl_grant_subject_type_valid
         CHECK (subject_type IN ('user', 'group'));

ALTER TABLE collab_documents DROP CONSTRAINT IF EXISTS collab_documents_resource_fk;

DROP TABLE IF EXISTS workspace_storage;
ALTER TABLE workspaces DROP COLUMN IF EXISTS storage_quota_bytes;

DROP TABLE IF EXISTS file_versions;

DROP TRIGGER IF EXISTS trg_files_updated_at ON files;
ALTER TABLE files
    DROP CONSTRAINT IF EXISTS files_type_valid,
    DROP COLUMN IF EXISTS updated_by,
    DROP COLUMN IF EXISTS updated_at,
    DROP COLUMN IF EXISTS trashed_by,
    DROP COLUMN IF EXISTS trashed_at,
    DROP COLUMN IF EXISTS current_version,
    DROP COLUMN IF EXISTS file_type,
    -- Last: drive_folders cannot be dropped while this FK exists.
    DROP COLUMN IF EXISTS folder_id;

DROP TRIGGER IF EXISTS trg_drive_folders_updated_at ON drive_folders;
DROP TABLE IF EXISTS drive_folders;

-- Re-materialize against the restored views so acl_object/acl_key match what a
-- 016-era database would hold.
INSERT INTO acl_object (object_type, object_id, workspace_id, path)
SELECT object_type, object_id, workspace_id, path FROM acl_object_expected
    ON CONFLICT (object_type, object_id) DO UPDATE
   SET workspace_id = EXCLUDED.workspace_id,
       path         = EXCLUDED.path,
       updated_at   = NOW()
 WHERE acl_object.workspace_id IS DISTINCT FROM EXCLUDED.workspace_id
    OR acl_object.path         IS DISTINCT FROM EXCLUDED.path;

DELETE FROM acl_key k
 WHERE NOT EXISTS (
     SELECT 1 FROM acl_key_expected e
      WHERE e.object_type = k.object_type AND e.object_id = k.object_id AND e.key = k.key
 );

INSERT INTO acl_key (object_type, object_id, key)
SELECT object_type, object_id, key FROM acl_key_expected
    ON CONFLICT DO NOTHING;
