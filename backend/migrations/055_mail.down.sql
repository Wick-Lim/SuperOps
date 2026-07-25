-- Reverse of 055. Lossy and worth saying plainly: dropping mail_messages
-- destroys the record of every conversation with every customer. The raw
-- RFC822 objects survive in the bucket — and become unreferenced, so the
-- collector will sweep them once StorageKeysPresent no longer names raw_key.
-- An operator rolling this back should archive the bucket first.

-- The views first: they reference mail_conversations, so dropping the tables
-- underneath a live view would fail. Restored to exactly what 025 and 028
-- defined.
DROP VIEW IF EXISTS acl_key_expected;
DROP VIEW IF EXISTS acl_object_expected;

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
    -- Precedence: folder, then the attached message's channel, then the
    -- workspace. The folder wins because it is the file's home — the channel
    -- attachment is a share OF that file, and it contributes a key (arm 7
    -- below) rather than a location. Before Drive there was no folder and the
    -- other two branches are byte-for-byte what migration 016 had.
    --
    -- The workspace equality on the folder join is not redundant: a
    -- cross-workspace folder_id is corrupt data, and inheriting its path would
    -- produce a row whose root segment disagrees with workspace_id — which
    -- acl_object_path_in_workspace then rejects, turning a data bug into a
    -- failed backfill. Degrading to the workspace path is the fail-closed
    -- reading, and it is what the channel branch already does.
    SELECT 'file'::TEXT, f.id, f.workspace_id,
           CASE
               WHEN fo.object_id IS NOT NULL
                   THEN fo.path || 'file:' || f.id::TEXT || '/'
               WHEN ch.id IS NOT NULL
                   THEN '/workspace:' || f.workspace_id::TEXT || '/channel:' || ch.id::TEXT
                        || '/file:' || f.id::TEXT || '/'
               ELSE '/workspace:' || f.workspace_id::TEXT || '/file:' || f.id::TEXT || '/'
           END
      FROM files f
      LEFT JOIN acl_object fo ON fo.object_type = 'folder'
                             AND fo.object_id   = f.folder_id
                             AND fo.workspace_id = f.workspace_id
      LEFT JOIN messages m    ON m.id = f.message_id
      LEFT JOIN channels ch   ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

CREATE VIEW acl_key_expected AS
    -- 1. The workspace object is readable by every member of the workspace.
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT AS key
      FROM acl_object o
     WHERE o.object_type = 'workspace'
UNION
    -- 2a. A channel is readable by its members, whatever its type.
    SELECT o.object_type, o.object_id, 'u-' || cm.user_id::TEXT
      FROM acl_object o
      JOIN channel_members cm ON cm.channel_id = o.object_id
     WHERE o.object_type = 'channel'
UNION
    -- 2b. ...and a PUBLIC channel is additionally readable by every member of
    --     its workspace.
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT
      FROM acl_object o
      JOIN channels c ON c.id = o.object_id
     WHERE o.object_type = 'channel' AND c.type = 'public'
UNION
    -- 3. Anything inside a container inherits the container's key.
    SELECT o.object_type, o.object_id, acl_container_key(acl_parent_segment(o.path))
      FROM acl_object o
     WHERE acl_container_key(acl_parent_segment(o.path)) IS NOT NULL
UNION
    -- 4. A file owned by neither a folder nor a channel is readable by its
    --    uploader alone.
    SELECT o.object_type, o.object_id, 'u-' || f.user_id::TEXT
      FROM acl_object o
      JOIN files f ON f.id = o.object_id
     WHERE o.object_type = 'file'
       AND split_part(coalesce(acl_parent_segment(o.path), ''), ':', 1) = 'workspace'
UNION
    -- 5. Explicit grants, inherited by the whole subtree of the granted object.
    --
    --    'link' is new in this migration and shares the 'g-' prefix. A sixth
    --    prefix would have to be added to internal/search's CLOSED validator,
    --    and a key that fails it is DROPPED from the filter — a dropped
    --    narrowing term WIDENS the query, which for a tenancy filter is a
    --    cross-tenant leak (README ruling 3). 'g-' already means "a subject that
    --    is not one person".
    SELECT d.object_type, d.object_id,
           CASE g.subject_type
               WHEN 'user'      THEN 'u-'
               WHEN 'group'     THEN 'g-'
               WHEN 'workspace' THEN 'w-'
               WHEN 'link'      THEN 'g-'
           END || g.subject_id::TEXT
      FROM acl_grant g
      JOIN acl_object go ON go.object_type = g.object_type AND go.object_id = g.object_id
      JOIN acl_object d  ON d.path LIKE go.path || '%'
     WHERE g.subject_type IN ('user', 'group', 'workspace', 'link')
UNION
    -- 6. A file attached to a message is readable by that channel, whatever its
    --    Drive home.
    SELECT 'file'::TEXT, f.id, 'c-' || ch.id::TEXT
      FROM files f
      JOIN messages m  ON m.id = f.message_id
      JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

DROP INDEX IF EXISTS idx_files_mail_message;
ALTER TABLE files DROP COLUMN IF EXISTS mail_message_id;

DROP INDEX IF EXISTS idx_files_unowned;
CREATE INDEX idx_files_unowned ON files (created_at)
    WHERE folder_id IS NULL AND message_id IS NULL AND trashed_at IS NULL;

DROP TABLE IF EXISTS mail_ingest_tokens;
DROP TABLE IF EXISTS mail_inbound_events;
DROP TABLE IF EXISTS mail_messages;
DROP TABLE IF EXISTS mail_conversations;
DROP TABLE IF EXISTS mailboxes;
DROP TABLE IF EXISTS mail_domains;
