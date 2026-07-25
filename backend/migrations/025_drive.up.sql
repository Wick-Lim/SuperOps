-- 025: Drive — folders, Drive-shaped files, versions, trash, quota.
--
-- See docs/plans/02-drive.md §3. This is the foundation commit: it ships with
-- the object-GC predicate fix in internal/file, and NOTHING may write a
-- drive_folders or a foldered files row before both are deployed together.
-- The reason is in plan 02 §2 and it is worth repeating here, because a reader
-- of this file alone would not guess it:
--
--   Before this migration, `files.message_id IS NULL` meant "garbage". The
--   object GC deletes those rows and their objects 24 hours after upload and
--   logs it as success. A Drive file has no message. Landing folders without
--   the predicate fix deletes every file a user uploads to Drive, silently,
--   the next day.
--
-- Four things here are load-bearing.
--
-- 1. folder_id and message_id are INDEPENDENT. A chat upload has folder_id
--    NULL; a Drive file has folder_id NOT NULL; a Drive file shared into a
--    channel has both. Every predicate that touches one must name all three of
--    folder_id / message_id / trashed_at — that ambiguity is the bug above.
--
-- 2. The file id is stable across versions. file_versions carries the object
--    keys; files.storage_key stays as the denormalized head pointer so the
--    download path and the GC do not move. That is what lets a message
--    attachment, a collab_documents.resource_id and a search document keep
--    pointing at the same thing when someone uploads a revision.
--
-- 3. Deletion is trash + a purge job, never a cascade. drive_folders.parent_id
--    is ON DELETE RESTRICT: one DELETE must not be able to remove an arbitrary
--    number of files with no object cleanup and no audit trail.
--
-- 4. The two acl_*_expected views are REPLACED, not extended in Go. They are
--    the single definition of derived ACL state (migration 016's header), and
--    acl_key's reconcile deletes any key the view does not produce — for every
--    object type, not just the derived three. A folder whose keys are written
--    explicitly and absent from the view would be readable until the hourly
--    verifier ran and then silently readable by nobody.

-- ---------------------------------------------------------------------------
-- Folders
-- ---------------------------------------------------------------------------

CREATE TABLE drive_folders (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- RESTRICT, not CASCADE: see header note 3.
    parent_id    UUID REFERENCES drive_folders(id) ON DELETE RESTRICT,
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 255),
    is_root      BOOLEAN NOT NULL DEFAULT FALSE,

    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    trashed_at   TIMESTAMPTZ,
    trashed_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT drive_folders_parent_required CHECK (is_root OR parent_id IS NOT NULL),
    -- A folder cannot be its own parent. Deeper cycles are a path-prefix check
    -- in the move handler; this catches the one case a constraint can.
    CONSTRAINT drive_folders_no_self_parent  CHECK (parent_id IS NULL OR parent_id <> id)
);

-- Exactly one root per workspace.
CREATE UNIQUE INDEX idx_drive_folders_root     ON drive_folders (workspace_id) WHERE is_root;
-- The children listing. Names are deliberately NOT unique within a parent: a
-- unique index fights restore-from-trash and copy, and Drive-style duplicate
-- names are the expected behaviour.
CREATE INDEX        idx_drive_folders_children ON drive_folders (parent_id, name) WHERE trashed_at IS NULL;
CREATE INDEX        idx_drive_folders_trash    ON drive_folders (workspace_id, trashed_at) WHERE trashed_at IS NOT NULL;

CREATE TRIGGER trg_drive_folders_updated_at
    BEFORE UPDATE ON drive_folders FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- files, reshaped
-- ---------------------------------------------------------------------------

ALTER TABLE files
    ADD COLUMN folder_id       UUID REFERENCES drive_folders(id) ON DELETE RESTRICT,
    -- Same shape rule as collab_documents.resource_type (migration 015) and as
    -- acl_object.object_type (016): a new editor must not need a migration, and
    -- the value is never spliced into a query, a key or a subject.
    ADD COLUMN file_type       TEXT NOT NULL DEFAULT 'file',
    ADD COLUMN current_version INT  NOT NULL DEFAULT 1 CHECK (current_version > 0),
    ADD COLUMN trashed_at      TIMESTAMPTZ,
    ADD COLUMN trashed_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD COLUMN updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ADD COLUMN updated_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    ADD CONSTRAINT files_type_valid CHECK (file_type ~ '^[a-z][a-z0-9_]{0,31}$');

CREATE INDEX idx_files_folder ON files (folder_id, created_at DESC, id DESC) WHERE trashed_at IS NULL;
CREATE INDEX idx_files_trash  ON files (workspace_id, trashed_at) WHERE trashed_at IS NOT NULL;
-- The GC predicate below is served by this: the collector wants the small set
-- of rows owned by neither a folder nor a message, oldest first.
CREATE INDEX idx_files_unowned ON files (created_at)
    WHERE folder_id IS NULL AND message_id IS NULL AND trashed_at IS NULL;

CREATE TRIGGER trg_files_updated_at
    BEFORE UPDATE ON files FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Versions
-- ---------------------------------------------------------------------------

CREATE TABLE file_versions (
    file_id      UUID   NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    version      INT    NOT NULL CHECK (version > 0),
    storage_key  TEXT   NOT NULL,
    size_bytes   BIGINT NOT NULL CHECK (size_bytes >= 0),
    content_type TEXT   NOT NULL,
    checksum     TEXT   NOT NULL DEFAULT '',  -- hex sha256, '' when not computed
    created_by   UUID   REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (file_id, version)
);

-- The bucket sweep's third arm joins on this. Without the index every sweep
-- run is a sequential scan of every version ever written.
CREATE INDEX idx_file_versions_key ON file_versions (storage_key);

-- One version row per existing file. Every file in the table today is at
-- version 1 by definition (nothing could create a second one), so this is
-- exact rather than approximate.
INSERT INTO file_versions (file_id, version, storage_key, size_bytes, content_type, created_by, created_at)
SELECT f.id, 1, f.storage_key, f.size_bytes, f.content_type, f.user_id, f.created_at
  FROM files f;

-- ---------------------------------------------------------------------------
-- Quota
-- ---------------------------------------------------------------------------

-- 0 = unlimited, which is what every existing workspace gets. A quota that
-- appeared retroactively and locked people out of their own Drive would be a
-- migration that takes a product decision on the operator's behalf.
ALTER TABLE workspaces ADD COLUMN storage_quota_bytes BIGINT NOT NULL DEFAULT 0
    CHECK (storage_quota_bytes >= 0);

CREATE TABLE workspace_storage (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Blob bytes. Maintained transactionally by the upload path, so it is exact
    -- at every instant an upload can observe it.
    bytes_used   BIGINT NOT NULL DEFAULT 0 CHECK (bytes_used >= 0),
    -- Bytes held by CRDT logs and snapshots, recomputed by the accounting job.
    -- Separate because it is eventually consistent and blob bytes are not:
    -- summing them into one column would make the exact half unauditable.
    collab_bytes BIGINT NOT NULL DEFAULT 0 CHECK (collab_bytes >= 0),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO workspace_storage (workspace_id, bytes_used)
SELECT w.id, COALESCE(SUM(f.size_bytes), 0)
  FROM workspaces w
  LEFT JOIN files f ON f.workspace_id = w.id
 GROUP BY w.id;

-- ---------------------------------------------------------------------------
-- The root folder, and the collab FK 015 left open on purpose
-- ---------------------------------------------------------------------------

INSERT INTO drive_folders (workspace_id, name, is_root, parent_id)
SELECT w.id, 'Drive', TRUE, NULL FROM workspaces w;

-- Folders are ACL-native (see the view section below), so their acl_object rows
-- are written explicitly rather than materialized from a view. This is the
-- backfill's copy of what authz.Register does for every folder created after
-- this migration; the root's parent is the workspace, so its path is one
-- segment deeper.
INSERT INTO acl_object (object_type, object_id, workspace_id, path)
SELECT 'folder', df.id, df.workspace_id,
       '/workspace:' || df.workspace_id::TEXT || '/folder:' || df.id::TEXT || '/'
  FROM drive_folders df
    ON CONFLICT (object_type, object_id) DO NOTHING;

-- 'workspace' as a grant subject: "everyone in this workspace".
--
-- This is what makes Drive shared, and it is deliberately a GRANT rather than a
-- rule. An ACL-native object is deny-by-default — that is the property plan 00
-- built and internal/authz's tests pin — so Drive needs something to open it,
-- and the only safe something is the mechanism that already exists. One row on
-- the root folder, inherited down the path like any other grant, resolved by
-- grantedCapability and materialized by acl_key_expected arm 5. No second
-- inheritance mechanism, and no way for the list path and the decision path to
-- drift apart.
--
-- The subject's key prefix is 'w-', which KeysFor already puts in every member's
-- key set. It is not a tenancy statement: acl_object.workspace_id decides
-- tenancy, and the path CHECK refuses an object that is not in the granting
-- workspace to begin with.
ALTER TABLE acl_grant
    DROP CONSTRAINT acl_grant_subject_type_valid,
    ADD  CONSTRAINT acl_grant_subject_type_valid
         CHECK (subject_type IN ('user', 'group', 'workspace'));

-- ADMIN, and it is less alarming than it reads: grantedCapability caps a
-- workspace grant at what the recipient's ROLE in that workspace is worth, so
-- this resolves to admin for an owner or admin, write for a member and read for
-- a guest — exactly the role ladder, with nobody promoted past their own role.
--
-- Granting `write` instead would flatten everyone at write, and then nobody
-- could ever share a Drive folder outside the workspace, because `share` sits
-- above `write` on the ladder and no path would reach it.
INSERT INTO acl_grant (object_type, object_id, subject_type, subject_id, capability)
SELECT 'folder', df.id, 'workspace', df.workspace_id, 'admin'
  FROM drive_folders df
 WHERE df.is_root
    ON CONFLICT DO NOTHING;

-- Migration 015 declared this FK missing "because Drive does not exist"; Drive
-- exists now. It also makes purge correct by construction: without it, deleting
-- a file leaves its update log behind — user-content Postgres bytes with
-- nothing pointing at them, invisible to the object GC because they are not
-- objects.
--
-- NOT VALID would let existing rows escape the check. There are none that can
-- fail (collab_documents is empty of file-typed rows on any deployment that has
-- not shipped Drive), so it is validated immediately and a violation is a bug
-- report rather than a silent skip.
ALTER TABLE collab_documents
    ADD CONSTRAINT collab_documents_resource_fk
    FOREIGN KEY (resource_id) REFERENCES files(id) ON DELETE CASCADE;

-- ---------------------------------------------------------------------------
-- The expected ACL state, replaced
-- ---------------------------------------------------------------------------
--
-- Two changes, and both are required by the tables above rather than optional:
--
--   * acl_object_expected learns that a file with a folder_id hangs off that
--     folder rather than off its message's channel.
--   * acl_key_expected learns the 'workspace' subject in arm 5, and one new
--     arm: a file attached to a message is readable by that channel WHATEVER
--     its Drive home (arm 6) — sharing a Drive file into a channel is an act of
--     sharing, and before this the channel key came from the path, which a
--     Drive file no longer has.
--
-- WHAT IS DELIBERATELY NOT HERE: an arm that makes Drive readable. Drive is
-- shared by an acl_grant row on the root folder whose subject is the workspace,
-- so arm 5 carries it and the decision path (grantedCapability) resolves the
-- same row. A view arm would have been a second inheritance mechanism that only
-- the list path knew about, which is a listing that shows what opening refuses.
--
-- WHAT IS DELIBERATELY ABSENT: a folder arm in acl_object_expected.
--
-- Folders are ACL-NATIVE. Their acl_object row is written by authz.Register and
-- moved by authz.Move, which is the whole point of the materialized path —
-- moving a folder with 20k descendants is one prefix rewrite. Putting folders
-- in this view would make them a derived type (internal/authz's derivedTypes,
-- which is the set the view defines), and Register and Move both refuse derived
-- types outright: their rows belong to the legacy schema and a hand-placed one
-- would be reverted by the next Rebuild. Drive would then have no way to move a
-- folder at all.
--
-- Files stay derived, so their path must be computable — and it is, from the
-- folder's already-materialized acl_object row. That is why the join below goes
-- to acl_object rather than walking drive_folders.parent_id recursively: one
-- join instead of a recursive CTE, and it reads the same path the folder
-- actually has rather than a second opinion about what it should be.
--
-- Dropped and recreated rather than CREATE OR REPLACE: the arms change shape,
-- and REPLACE only accepts an identical column list, which would fail at a
-- moment when a half-applied view is the worst possible outcome.

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
    -- 3. Anything inside a container inherits the container's key — the bounded
    --    case: one key per object no matter how many people may read the
    --    container.
    SELECT o.object_type, o.object_id, acl_container_key(acl_parent_segment(o.path))
      FROM acl_object o
     WHERE acl_container_key(acl_parent_segment(o.path)) IS NOT NULL
UNION
    -- 4. A file owned by neither a folder nor a channel is readable by its
    --    uploader alone. Unchanged from 016; a Drive file's parent segment is
    --    'folder:', so this no longer fires for it.
    SELECT o.object_type, o.object_id, 'u-' || f.user_id::TEXT
      FROM acl_object o
      JOIN files f ON f.id = o.object_id
     WHERE o.object_type = 'file'
       AND split_part(coalesce(acl_parent_segment(o.path), ''), ':', 1) = 'workspace'
UNION
    -- 5. Explicit grants, inherited by the whole subtree of the granted object.
    --
    --    'workspace' is new here and it is what makes Drive shared: one grant on
    --    the Drive root, carried down by this arm like any other. Its key is
    --    w-<workspace>, which every member already holds — so nothing had to
    --    learn a new prefix, and derivedCapability did not have to learn about
    --    folders at all.
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
    -- 6. A file attached to a message is readable by that channel, whatever its
    --    Drive home. For a chat upload this restates arm 3; for a Drive file
    --    shared into a channel it is the only arm that says so, because the
    --    file's path names its folder.
    SELECT 'file'::TEXT, f.id, 'c-' || ch.id::TEXT
      FROM files f
      JOIN messages m  ON m.id = f.message_id
      JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

-- ---------------------------------------------------------------------------
-- Re-materialize
-- ---------------------------------------------------------------------------
--
-- The same three statements migration 016 ends with and internal/authz's
-- Rebuild runs. They are re-runnable by construction; running them here is what
-- registers the root folders created above and rewrites the file paths whose
-- definition just changed.

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
