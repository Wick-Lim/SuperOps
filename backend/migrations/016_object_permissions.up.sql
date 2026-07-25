-- 016: object-level permissions — the foundation, not the cutover.
--
-- See docs/plans/00-permissions.md. Three tables, and nothing in the product
-- reads them yet: handlers keep calling the fifteen membership methods in
-- internal/authz, and this schema is materialized alongside them so the
-- comparison in step 3 of the plan's migration section can prove the two agree
-- before anything is switched over.
--
-- Four decisions are load-bearing and are why the DDL looks like this.
--
-- 1. The hierarchy is a materialized path, and the path INCLUDES the object's
--    own segment and ends in '/'. That makes a subtree exactly
--    `path LIKE <prefix> || '%'` — one predicate, served by a B-tree with
--    text_pattern_ops — and makes a move a prefix rewrite rather than
--    O(subtree x depth) closure-row churn.
--
-- 2. object_type is restricted to ^[a-z][a-z0-9]{0,31}$ — no underscore. Every
--    character that may appear in a path is therefore alphanumeric, '-', ':'
--    or '/', and NONE of them is a LIKE metacharacter. A '_' in a type name
--    would make every subtree predicate a single-character wildcard, i.e. a
--    subtree that quietly matches siblings. Excluding it from the alphabet is
--    cheaper and more obviously correct than escaping at fifteen call sites.
--
-- 3. A path always begins with its own workspace segment, enforced by a CHECK
--    against workspace_id in the same row. acl_object.workspace_id stays
--    authoritative for tenancy (plan 00, "Sharing outside the workspace"), so
--    no move and no grant can relocate an object into another tenant without
--    the row failing to write.
--
-- 4. acl_key stores EXACTLY the string internal/search's validKey accepts:
--    '<prefix>-<uuid>' over the closed set {w-, c-, u-, g-, f-}. The
--    Meilisearch filter is built by string concatenation and a key that fails
--    validation is DROPPED — and a dropped narrowing term widens the query,
--    which for a tenancy filter is a cross-tenant leak. The CHECK below is the
--    database's half of that contract.
--
-- Deny is absence. There is deliberately no negative grant: effective
-- capability is a max over the grants that exist, so "why can this person not
-- see it" is answered by looking at a set rather than by evaluating an ordered
-- rule list. See the plan for why that was excluded on purpose.

-- ---------------------------------------------------------------------------
-- acl_object — the hierarchy
-- ---------------------------------------------------------------------------

CREATE TABLE acl_object (
    object_type  TEXT NOT NULL,
    object_id    UUID NOT NULL,
    -- Every object belongs to exactly one workspace, and this is the column
    -- tenancy is decided by. ON DELETE CASCADE because an ACL for a deleted
    -- workspace is not merely stale, it is an authorization decision about
    -- rows that no longer exist.
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    -- Materialized path, own segment included, trailing slash:
    --   '/workspace:<uuid>/'
    --   '/workspace:<uuid>/channel:<uuid>/'
    --   '/workspace:<uuid>/channel:<uuid>/file:<uuid>/'
    path         TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (object_type, object_id),

    CONSTRAINT acl_object_type_shape CHECK (object_type ~ '^[a-z][a-z0-9]{0,31}$'),
    -- The path is a sequence of '<type>:<canonical lowercase uuid>' segments.
    -- Rejecting anything else here is what lets every consumer treat a path as
    -- a safe prefix rather than as user input.
    CONSTRAINT acl_object_path_shape CHECK (
        path ~ '^(/[a-z][a-z0-9]{0,31}:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})+/$'
    ),
    -- Depth cap (plan 00, "Deep hierarchies"): 32 segments. A path has one
    -- more '/' than it has segments.
    CONSTRAINT acl_object_path_depth CHECK (
        length(path) - length(replace(path, '/', '')) <= 33
    ),
    -- Tenancy: the root segment is this row's own workspace. uuid::text is
    -- canonical lowercase and contains no LIKE metacharacter.
    CONSTRAINT acl_object_path_in_workspace CHECK (
        path LIKE ('/workspace:' || workspace_id::text || '/%')
    )
);

-- The subtree predicate. text_pattern_ops is what makes `path LIKE 'prefix%'`
-- an index range scan under any collation; the default opclass only serves it
-- under C collation.
CREATE INDEX idx_acl_object_path ON acl_object (path text_pattern_ops);
CREATE INDEX idx_acl_object_workspace ON acl_object (workspace_id, object_type);

-- ---------------------------------------------------------------------------
-- acl_grant — the exceptions
-- ---------------------------------------------------------------------------

CREATE TABLE acl_grant (
    object_type  TEXT NOT NULL,
    object_id    UUID NOT NULL,
    -- 'user' today; 'group' is reserved (matching the g- key prefix) and a
    -- share-link token is the eventual third. The column exists so adding one
    -- is not a rewrite.
    subject_type TEXT NOT NULL,
    subject_id   UUID NOT NULL,
    capability   TEXT NOT NULL,
    granted_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- One row per (object, subject): a subject holds ONE capability on an
    -- object, the highest granted. Storing several and taking the max invites
    -- disagreement about which is current.
    PRIMARY KEY (object_type, object_id, subject_type, subject_id),
    FOREIGN KEY (object_type, object_id)
        REFERENCES acl_object (object_type, object_id) ON DELETE CASCADE,

    CONSTRAINT acl_grant_capability_valid
        CHECK (capability IN ('read', 'comment', 'write', 'share', 'admin')),
    CONSTRAINT acl_grant_subject_type_valid
        CHECK (subject_type IN ('user', 'group'))
);

-- The audit path: "this person left the company, what did they have access to".
CREATE INDEX idx_acl_grant_subject ON acl_grant (subject_type, subject_id);

-- ---------------------------------------------------------------------------
-- acl_key — the read-path materialization
-- ---------------------------------------------------------------------------

CREATE TABLE acl_key (
    object_type TEXT NOT NULL,
    object_id   UUID NOT NULL,
    key         TEXT NOT NULL,

    -- Write path: every mutation rewrites one object's (or one subtree's) keys,
    -- so the primary key's leading columns are the object.
    PRIMARY KEY (object_type, object_id, key),
    FOREIGN KEY (object_type, object_id)
        REFERENCES acl_object (object_type, object_id) ON DELETE CASCADE,

    -- Exactly internal/search's validKey: a closed prefix set and a canonical
    -- lowercase uuid. f- (folder) is included ahead of Drive so the validator
    -- never has to be widened by a pillar in a hurry.
    CONSTRAINT acl_key_shape CHECK (
        key ~ '^[wcugf]-[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$'
    )
);

-- Read path: "every object whose key set intersects the caller's key set", one
-- indexed query instead of N permission checks. The object columns are in the
-- index so the lookup is index-only.
CREATE INDEX idx_acl_key_lookup ON acl_key (key, object_type, object_id);

-- ---------------------------------------------------------------------------
-- Path helpers
-- ---------------------------------------------------------------------------

-- acl_parent_segment returns the second-to-last segment of a path — the
-- object's immediate parent — or NULL for a root (workspace) object.
CREATE FUNCTION acl_parent_segment(p TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE STRICT PARALLEL SAFE AS $$
    SELECT (regexp_match(
        p,
        '/([a-z][a-z0-9]{0,31}:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})/[a-z][a-z0-9]{0,31}:[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/$'
    ))[1]
$$;

-- acl_container_key maps a path segment to the access key that grants read on
-- everything inside it, or NULL when the segment names something that is not a
-- container. A workspace is deliberately NOT a container here: 'w-' means
-- "member of the workspace", which is not the same statement as "may read
-- everything in it", and conflating the two is how a private channel becomes
-- workspace-readable.
CREATE FUNCTION acl_container_key(segment TEXT) RETURNS TEXT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE split_part(segment, ':', 1)
        WHEN 'channel' THEN 'c-' || split_part(segment, ':', 2)
        WHEN 'folder'  THEN 'f-' || split_part(segment, ':', 2)
    END
$$;

-- ---------------------------------------------------------------------------
-- The expected state — ONE definition, used by the backfill, by every
-- mutation's key rewrite, and by the drift verifier
-- ---------------------------------------------------------------------------
--
-- These views are the definition of what acl_object and acl_key must contain.
-- The backfill below is `INSERT ... SELECT FROM <view>`; internal/authz's
-- Rebuild runs the same statements; the worker's drift verifier diffs the
-- tables against the same views. A second, hand-written copy of this logic in
-- Go would be a verifier that reports its own disagreement with the backfill
-- as production drift, which is worse than no verifier at all.

-- acl_object_expected covers only the types whose hierarchy is DERIVED from the
-- pre-ACL schema: workspaces, channels and files. Objects owned by the ACL
-- layer itself (folders, docs, sheets, ... once Drive lands) are created
-- explicitly and are not in this view — which is why Rebuild only reconciles
-- the three derived types and never deletes anything else.
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
    -- A file hangs off the channel of the message it is attached to, exactly as
    -- file.Handler.canRead authorizes it. An unattached file (or one whose
    -- message was hard-deleted, since files.message_id is ON DELETE SET NULL)
    -- hangs off the workspace and is readable by its uploader alone.
    --
    -- The channel join is constrained to the file's own workspace: a
    -- cross-workspace message_id is corrupt data, and degrading it to
    -- "uploader only" is the fail-closed reading.
    SELECT 'file'::TEXT, f.id, f.workspace_id,
           CASE WHEN ch.id IS NULL
                THEN '/workspace:' || f.workspace_id::TEXT || '/file:' || f.id::TEXT || '/'
                ELSE '/workspace:' || f.workspace_id::TEXT || '/channel:' || ch.id::TEXT
                     || '/file:' || f.id::TEXT || '/'
           END
      FROM files f
      LEFT JOIN messages m  ON m.id = f.message_id
      LEFT JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id;

-- acl_key_expected is the set of access keys that grant READ on each registered
-- object. Read is the lowest capability, so every grant contributes regardless
-- of its capability — no ranking appears here on purpose.
--
-- Rules 1-4 are the derived layer: they transcribe the predicates that
-- internal/authz already enforces (IsWorkspaceMember, CanReadChannel,
-- file.Handler.canRead) into keys. Rule 5 is the ACL-native layer and is the
-- only one that will survive the cutover unchanged.
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
    --     its workspace. Not by an authenticated stranger: that gap is what
    --     allowed cross-tenant browsing, and 'w-' is scoped to the workspace
    --     the object itself belongs to.
    SELECT o.object_type, o.object_id, 'w-' || o.workspace_id::TEXT
      FROM acl_object o
      JOIN channels c ON c.id = o.object_id
     WHERE o.object_type = 'channel' AND c.type = 'public'
UNION
    -- 3. Anything inside a container inherits the container's key — this is the
    --    bounded case: one key per object no matter how many people may read
    --    the container. An attached file gets 'c-<channel>', which is
    --    byte-for-byte the ACL search.FileDoc already writes to Meilisearch.
    SELECT o.object_type, o.object_id, acl_container_key(acl_parent_segment(o.path))
      FROM acl_object o
     WHERE acl_container_key(acl_parent_segment(o.path)) IS NOT NULL
UNION
    -- 4. An unattached file is readable by its uploader alone.
    SELECT o.object_type, o.object_id, 'u-' || f.user_id::TEXT
      FROM acl_object o
      JOIN files f ON f.id = o.object_id
     WHERE o.object_type = 'file'
       AND split_part(coalesce(acl_parent_segment(o.path), ''), ':', 1) = 'workspace'
UNION
    -- 5. Explicit grants, inherited by the whole subtree of the granted object.
    --    The join is driven from acl_grant (empty today), so the LIKE runs once
    --    per grant against idx_acl_object_path rather than once per object.
    SELECT d.object_type, d.object_id,
           CASE g.subject_type WHEN 'user' THEN 'u-' WHEN 'group' THEN 'g-' END
               || g.subject_id::TEXT
      FROM acl_grant g
      JOIN acl_object go ON go.object_type = g.object_type AND go.object_id = g.object_id
      JOIN acl_object d  ON d.path LIKE go.path || '%'
     WHERE g.subject_type IN ('user', 'group');

-- ---------------------------------------------------------------------------
-- Backfill
-- ---------------------------------------------------------------------------
--
-- Re-runnable by construction, and re-run by internal/authz's Rebuild with
-- these exact statements. Order matters: objects, then a reconcile of keys
-- (delete-then-insert rather than truncate, so a concurrent reader never sees
-- an object with no keys and concludes it is unreadable).

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
