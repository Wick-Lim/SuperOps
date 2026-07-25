-- 028: sharing — per-object grants to people, and read-only links.
--
-- Grants need no schema: acl_grant already carries (object, subject,
-- capability) and internal/authz already inherits it down the subtree. What is
-- new is a THIRD subject type and the table behind it.
--
-- A LINK IS A SUBJECT, not a parallel permission system. Resolving one mints a
-- session whose key set is exactly `g-<link uuid>` — never the workspace member
-- key — so a link can widen what one object grants and can never widen tenancy,
-- because acl_object.workspace_id stays authoritative and the path CHECK refuses
-- an object outside the granting workspace.
--
-- It reuses the `g-` prefix rather than adding a sixth. internal/search's
-- validKey accepts a CLOSED set {w-,c-,u-,g-,f-} and a key that fails validation
-- is DROPPED from the filter — and a dropped narrowing term WIDENS the query,
-- which for a tenancy filter is a cross-tenant leak. Widening that validator to
-- ship a link is exactly the change docs/plans/README.md ruling 3 exists to
-- prevent, and `g-` already means "a subject that is not one person".

ALTER TABLE acl_grant
    DROP CONSTRAINT acl_grant_subject_type_valid,
    ADD  CONSTRAINT acl_grant_subject_type_valid
         CHECK (subject_type IN ('user', 'group', 'workspace', 'link'));

CREATE TABLE drive_share_links (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    object_type   TEXT NOT NULL CHECK (object_type IN ('file', 'folder')),
    object_id     UUID NOT NULL,

    -- sha256 of the token, never the token.
    --
    -- The token is a BEARER CREDENTIAL FOR CONTENT: anyone holding it reads the
    -- object. It is returned once, at creation, and cannot be shown again —
    -- which is the point, because a table an operator can read is a table an
    -- operator can read every share link out of. (webhooks.token in migration
    -- 007 stores plaintext; that is a wart to fix, not a precedent to follow.)
    token_hash    BYTEA NOT NULL UNIQUE,

    -- Read or comment. NOT write: the abuse surface of an anonymous writer is
    -- not worth v1 (plan 02 §13).
    capability    TEXT NOT NULL DEFAULT 'read' CHECK (capability IN ('read', 'comment')),

    -- bcrypt, or '' for no password. A second factor on a URL that may be
    -- forwarded further than intended.
    password_hash TEXT NOT NULL DEFAULT '',

    expires_at    TIMESTAMPTZ,
    max_uses      INT CHECK (max_uses IS NULL OR max_uses > 0),
    use_count     INT NOT NULL DEFAULT 0,

    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    revoked_at    TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The link's authorization lives in acl_grant, keyed by this row's id as the
    -- subject. The FK is what makes revocation total: dropping the acl_object
    -- row when an object is purged takes the grant AND, through this, the link.
    FOREIGN KEY (object_type, object_id)
        REFERENCES acl_object (object_type, object_id) ON DELETE CASCADE
);

-- Resolution: one indexed probe on the hash.
CREATE UNIQUE INDEX idx_drive_share_links_token ON drive_share_links (token_hash);
-- The sharing panel: every live link on one object.
CREATE INDEX idx_drive_share_links_object ON drive_share_links (object_type, object_id)
    WHERE revoked_at IS NULL;

-- ---------------------------------------------------------------------------
-- acl_key_expected, arm 5
-- ---------------------------------------------------------------------------
--
-- The view's grant arm maps a subject type to a key prefix through a CASE, and
-- a CASE with no matching branch yields NULL — so without this a link grant
-- materializes a NULL key, the reconcile INSERT fails, and creating a link 500s.
--
-- Recreated rather than altered because a view's body cannot be patched, and
-- this migration is sequenced LAST in the Drive block for exactly that reason:
-- it copies arms 1-4 and 6-7 from 025's still-current text, and a lost arm is
-- the listing-versus-opening split docs/plans/README.md ruling 5 exists to
-- prevent. If another migration in this block changes the view, this file has
-- to be rebased onto it rather than merged with it.

DROP VIEW acl_key_expected;

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

-- Re-materialize, so a deployment that already had grants converges now rather
-- than at the next hourly reconcile.
DELETE FROM acl_key k
 WHERE NOT EXISTS (
     SELECT 1 FROM acl_key_expected e
      WHERE e.object_type = k.object_type AND e.object_id = k.object_id AND e.key = k.key
 );

INSERT INTO acl_key (object_type, object_id, key)
SELECT object_type, object_id, key FROM acl_key_expected
    ON CONFLICT DO NOTHING;
