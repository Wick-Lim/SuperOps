-- Reverses 028.
--
-- LOSSY: every share link is destroyed, and the URLs already handed out stop
-- working. There is no way around that — the table IS the links — and it is
-- stated here rather than discovered by the people holding them.

-- The grants must go before the CHECK is narrowed, or the ALTER fails on rows it
-- can no longer describe.
DELETE FROM acl_grant WHERE subject_type = 'link';

-- Restore acl_key_expected to 025's text: arm 5 without the link branch.
DROP VIEW acl_key_expected;

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
           CASE g.subject_type
               WHEN 'user'      THEN 'u-'
               WHEN 'group'     THEN 'g-'
               WHEN 'workspace' THEN 'w-'
           END || g.subject_id::TEXT
      FROM acl_grant g
      JOIN acl_object go ON go.object_type = g.object_type AND go.object_id = g.object_id
      JOIN acl_object d  ON d.path LIKE go.path || '%'
     WHERE g.subject_type IN ('user', 'group', 'workspace')
UNION
    SELECT 'file'::TEXT, f.id, 'c-' || ch.id::TEXT
      FROM files f
      JOIN messages m  ON m.id = f.message_id
      JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

DROP TABLE IF EXISTS drive_share_links;

ALTER TABLE acl_grant
    DROP CONSTRAINT acl_grant_subject_type_valid,
    ADD  CONSTRAINT acl_grant_subject_type_valid
         CHECK (subject_type IN ('user', 'group', 'workspace'));
