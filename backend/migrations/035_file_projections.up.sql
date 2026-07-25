-- 035: the editor projection.
--
-- ONE table, product-wide, for every client-projected kind. Docs is the first
-- consumer; the spreadsheet and the design surface add nothing to it.
--
-- EVERYTHING HERE IS DERIVED. The truth is collab_updates. The server cannot
-- read a CRDT document — migration 015's header refuses a Go-side merge, and a
-- Go-side READER is the same class of work with the same failure mode — so the
-- client that already has the document in memory renders it to text and posts
-- that. Six product features need a readable body (search, mobile render, link
-- preview, export, mentions, backlinks) and not one of them can be served from
-- a BYTEA column of opaque updates.
--
-- The drop test for any proposed use of this data: would corrupting it lose a
-- user's writing? If yes, the design is wrong. Losing this whole table costs
-- search and mobile rendering until re-projection, and costs zero content.

CREATE TABLE file_projections (
    file_id      UUID PRIMARY KEY REFERENCES files(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL    REFERENCES workspaces(id) ON DELETE CASCADE,

    -- The log position this projection describes. The write is conditional on
    -- this increasing, which is what makes two clients projecting the same
    -- document race harmlessly: the loser matches zero rows. Broadcast order is
    -- not commit order, so without it a slow projector could overwrite a newer
    -- body with an older one.
    projection_seq BIGINT NOT NULL DEFAULT 0 CHECK (projection_seq >= 0),

    -- The block schema the extractor was compiled against. A projection from a
    -- client older than the stored version is REFUSED, not merged: an old
    -- extractor silently drops nodes it does not know, so accepting it would
    -- write a lossy body and a wrong ref set into the search index.
    schema_version INT NOT NULL DEFAULT 1 CHECK (schema_version > 0),

    body_text TEXT NOT NULL DEFAULT '',
    -- sha256 of body_text, so the indexer can skip an unchanged body. Five
    -- people idling in a busy document produce five projections; four of them
    -- are byte-identical and must not each cost a Meilisearch write.
    body_hash TEXT NOT NULL DEFAULT ''
              CHECK (body_hash = '' OR body_hash ~ '^[0-9a-f]{64}$'),

    -- [{block_id, level, text}] — the heading outline, for the document's
    -- table of contents and for a link preview.
    outline JSONB NOT NULL DEFAULT '[]'::jsonb,

    projected_at TIMESTAMPTZ,

    -- A projection is attacker-supplied by definition: it arrives over HTTP
    -- from a caller with write capability, describing content the server cannot
    -- verify. Every bound below is a 400 at the handler and a refusal here, so
    -- a client that skips the handler cannot make the row unbounded either.
    --
    -- 1 MiB of extracted text is roughly 250 printed pages. Past that the
    -- document is being used as a database and the projection is not the
    -- problem.
    CONSTRAINT file_projections_body_bounded    CHECK (octet_length(body_text) <= 1048576),
    CONSTRAINT file_projections_outline_array   CHECK (jsonb_typeof(outline) = 'array'),
    CONSTRAINT file_projections_outline_bounded CHECK (pg_column_size(outline) <= 262144)
);

-- Every typed object the document points at: an embedded file, another
-- document, an issue, an @mention. Rebuilt wholesale inside the projection
-- transaction, so it is exactly as fresh as the projection and never more.
--
-- IT AUTHORIZES NOTHING. It is a stored claim by a client, and it is used only
-- to find candidates — each of which is checked against the CALLER's capability
-- when it is resolved.
CREATE TABLE file_projection_refs (
    file_id UUID NOT NULL REFERENCES file_projections(file_id) ON DELETE CASCADE,

    -- internal/authz's type shape, NOT files.file_type's. A ref_type is fed
    -- straight to the resolver's object constructor, and authz forbids '_'
    -- because an object path is spliced into LIKE predicates.
    ref_type TEXT NOT NULL CHECK (ref_type ~ '^[a-z][a-z0-9]{0,31}$'),
    ref_id   UUID NOT NULL,

    -- Which block the reference sits in, so a backlink can say where. Not a
    -- uuid: a block id is the editor's, and the editor is the only thing that
    -- can interpret it.
    block_id TEXT NOT NULL DEFAULT '' CHECK (char_length(block_id) <= 64),

    PRIMARY KEY (file_id, ref_type, ref_id, block_id)
);

-- The reverse lookup: "which documents embed this file / mention this user".
CREATE INDEX idx_file_projection_refs_target
    ON file_projection_refs (ref_type, ref_id, file_id);

-- The anchor half of a comment on a range of a document.
--
-- The comment itself — body, thread, author, resolution, notifications — is
-- migration 030's row, unchanged. Only the position is new, because a position
-- in a CRDT is not an integer offset: concurrent edits move it.
--
-- Deliberately carries NO document id. comments already holds (object_type,
-- object_id) with a real FK to acl_object, and "every anchor in this document"
-- is a join served by idx_comments_object. A third copy of a fact two tables
-- already agree on is a third thing that can disagree.
CREATE TABLE comment_anchors (
    comment_id UUID PRIMARY KEY REFERENCES comments(id) ON DELETE CASCADE,

    -- Yjs encoded relative positions. OPAQUE: the server never decodes one, for
    -- the same reason it never merges an update.
    anchor_start BYTEA NOT NULL CHECK (octet_length(anchor_start) BETWEEN 1 AND 4096),
    anchor_end   BYTEA NOT NULL CHECK (octet_length(anchor_end)   BETWEEN 1 AND 4096),

    -- The quoted text at creation time. Denormalized on purpose: it is the only
    -- thing left when the anchored range is deleted, and an orphaned comment
    -- must stay readable or resolving a discussion destroys its subject.
    quote    TEXT NOT NULL DEFAULT '' CHECK (char_length(quote) <= 2000),
    block_id TEXT NOT NULL DEFAULT '' CHECK (char_length(block_id) <= 64),

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Documents whose projection has fallen behind, oldest-touched first. The
-- reconciler's only query.
CREATE INDEX idx_collab_documents_updated ON collab_documents (updated_at);
