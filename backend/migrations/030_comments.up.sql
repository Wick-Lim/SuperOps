-- 030: the comment surface — ONE table, product-wide.
--
-- It is numbered first in the work-tracking block on purpose. Four plans consume
-- it (02 Drive, 03 issues, 04 docs, 05 spreadsheets), and plan 02 §13 CUT
-- per-file comments explicitly on the grounds that "Phase 2 builds the comment
-- surface once and Drive adopts it". A second comment system is the §1 failure
-- the whole all-in-one thesis is written against, so this lands alone, before
-- the pillar that happens to be its first consumer.
--
-- THE TARGET IS AN acl_object, and that is the entire design.
--
-- acl_object's primary key is (object_type, object_id) — a polymorphic pair that
-- ALREADY exists, already carries a workspace, and already carries a
-- materialized path. Keying comments to it means:
--
--   * a comment is authorized by its TARGET with no rule of its own. "May I read
--     this comment" is "may I read the thing it is about", which is one question
--     the checker already answers;
--   * the FK is real. A polymorphic (type, id) pair with no constraint is the
--     usual shape and it rots — rows survive their target and nothing notices.
--     Here ON DELETE CASCADE means purging a Drive file takes its comments, and
--     that is enforced by the database rather than by whoever remembers;
--   * a new commentable type is a new acl_object type, which is a thing pillars
--     already create. It is not a migration here.
--
-- drive_share_links (migration 028) is the shipped precedent for the same
-- composite foreign key.

CREATE TABLE comments (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),

    -- Denormalized from acl_object so every listing and every tenancy filter is
    -- one table. It is NOT the authority — acl_object.workspace_id is — and the
    -- CHECK below is what stops the two disagreeing.
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    object_type  TEXT NOT NULL,
    object_id    UUID NOT NULL,

    author_id    UUID REFERENCES users(id) ON DELETE SET NULL,

    -- ONE LEVEL OF THREADING, and no more.
    --
    -- parent_id may only name a comment whose own parent_id IS NULL, enforced by
    -- the trigger below. Arbitrary nesting is a rendering problem with no good
    -- answer on a phone and a recursive query on every read; a flat list loses
    -- the "replying to" relation that makes a long thread readable at all. One
    -- level is what every product a user has met does.
    parent_id    UUID REFERENCES comments(id) ON DELETE CASCADE,

    body         TEXT NOT NULL,

    -- SOFT delete. A deleted comment leaves a tombstone rather than a hole,
    -- because the replies under it still make sense and a thread that silently
    -- loses its first message reads as corruption. body is blanked at delete
    -- time so the text is genuinely gone; the row survives to hold the replies.
    deleted_at   TIMESTAMPTZ,
    deleted_by   UUID REFERENCES users(id) ON DELETE SET NULL,

    -- Set on the first edit, so a client can say "edited" without comparing
    -- timestamps that a trigger moves for unrelated reasons.
    edited_at    TIMESTAMPTZ,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The target must exist. Without this the table accumulates comments on
    -- objects that were deleted years ago, and every reader has to defend
    -- against it.
    FOREIGN KEY (object_type, object_id)
        REFERENCES acl_object (object_type, object_id) ON DELETE CASCADE,

    -- A reply cannot be its own parent. Deeper cycles are impossible because
    -- depth is capped at one.
    CONSTRAINT comments_no_self_parent CHECK (parent_id IS NULL OR parent_id <> id),

    -- A LIVE comment has text; a deleted one has none.
    --
    -- The two halves are one constraint because they are one rule. Written as a
    -- plain `length(body) BETWEEN 1 AND 16000` it refuses the soft delete —
    -- which blanks the body on purpose, so that "deleted" means the text is
    -- genuinely gone rather than merely hidden from one query.
    CONSTRAINT comments_body_shape CHECK (
        (deleted_at IS NULL     AND length(body) BETWEEN 1 AND 16000)
     OR (deleted_at IS NOT NULL AND body = '')
    )
);

-- The thread listing: one object's comments, oldest first, which is how a
-- conversation reads. (created_at, id) is the keyset every other paginated
-- endpoint in the product uses.
CREATE INDEX idx_comments_object ON comments (object_type, object_id, created_at, id);

-- The replies under one comment.
CREATE INDEX idx_comments_parent ON comments (parent_id, created_at)
    WHERE parent_id IS NOT NULL;

-- "What has this person been saying" — the audit direction, and the one a
-- departing-employee review asks for.
CREATE INDEX idx_comments_author ON comments (author_id, created_at DESC)
    WHERE author_id IS NOT NULL;

CREATE TRIGGER trg_comments_updated_at
    BEFORE UPDATE ON comments FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- The two invariants a CHECK constraint cannot express
-- ---------------------------------------------------------------------------
--
-- Both are enforced here rather than in Go because both are the kind of thing
-- that is correct in the handler on the day it is written and wrong the second
-- time somebody inserts a row.

CREATE FUNCTION comments_enforce_shape() RETURNS TRIGGER AS $$
DECLARE
    target_workspace   UUID;
    parent_parent      UUID;
    parent_object_type TEXT;
    parent_object_id   UUID;
BEGIN
    -- 1. The denormalized workspace is the target's workspace. A comment whose
    --    workspace_id disagreed with its object's would be visible in a tenancy
    --    filter that scans comments and invisible to one that joins acl_object,
    --    which is the shape of every cross-tenant leak in this codebase.
    SELECT o.workspace_id INTO target_workspace
      FROM acl_object o
     WHERE o.object_type = NEW.object_type AND o.object_id = NEW.object_id;

    IF target_workspace IS NULL THEN
        RAISE EXCEPTION 'comment target %:% is not a registered object',
            NEW.object_type, NEW.object_id;
    END IF;
    IF NEW.workspace_id <> target_workspace THEN
        RAISE EXCEPTION 'comment workspace % disagrees with target %:% (workspace %)',
            NEW.workspace_id, NEW.object_type, NEW.object_id, target_workspace;
    END IF;

    -- 2. One level of threading, and the reply is on the SAME object.
    IF NEW.parent_id IS NOT NULL THEN
        SELECT c.parent_id, c.object_type, c.object_id
          INTO parent_parent, parent_object_type, parent_object_id
          FROM comments c WHERE c.id = NEW.parent_id;

        IF NOT FOUND THEN
            RAISE EXCEPTION 'comment parent % does not exist', NEW.parent_id;
        END IF;
        IF parent_parent IS NOT NULL THEN
            RAISE EXCEPTION 'comments nest one level; % is already a reply', NEW.parent_id;
        END IF;
        IF parent_object_type <> NEW.object_type
           OR parent_object_id <> NEW.object_id THEN
            -- A reply on a different object than its parent would appear in one
            -- thread and be counted in another.
            RAISE EXCEPTION 'a reply must be on the same object as its parent';
        END IF;
    END IF;

    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_comments_shape
    BEFORE INSERT OR UPDATE OF object_type, object_id, parent_id, workspace_id
    ON comments FOR EACH ROW EXECUTE FUNCTION comments_enforce_shape();
