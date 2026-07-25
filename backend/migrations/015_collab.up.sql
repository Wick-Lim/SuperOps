-- 015: The collaboration layer — an append-only CRDT update log with snapshots.
--
-- Three editors (docs, spreadsheet, design surface) need several people editing
-- one object at once. The CRDT lives in the client (Yjs); this schema stores and
-- orders opaque update blobs and never interprets one. There is deliberately no
-- server-side merge: a Go re-implementation that disagreed with the client's
-- would be a corruption bug debuggable from neither side.
--
-- Three properties are load-bearing and are why the tables look like this:
--
-- 1. head_seq lives on the document row, not in a SEQUENCE. Every append does
--    `UPDATE collab_documents SET head_seq = head_seq + 1 ... RETURNING head_seq`
--    as the first statement of its transaction, so the row lock serialises
--    appends to one document and the log has no gaps and no holes that are
--    "allocated but not yet committed". A SEQUENCE would hand out 5 to a
--    transaction that commits after the one holding 6, and a reader that saw 6
--    would conclude 5 does not exist. Clients advance their watermark over a
--    contiguous prefix, so a hole would stall every later update.
--
-- 2. Compaction cannot race a writer into losing an update, for the same
--    reason. Compaction takes the same document row lock (SELECT ... FOR UPDATE)
--    before it reads head_seq, so an append that has not committed yet is one
--    the compactor is blocked behind — it can never delete an update that is
--    still in flight. That is the whole reason head_seq is a locked counter
--    rather than a derived MAX(seq).
--
-- 3. Snapshots are a small history, not a single row. A snapshot is produced by
--    a *client* (the server cannot merge CRDT state), so a buggy or hostile
--    client can submit a bad one. Keeping the last few means the previous good
--    state is still on disk and recoverable, and it costs a bounded amount.
--
-- Awareness — cursors, selections, who is here — is deliberately absent from
-- this file. It is ephemeral, rides the WebSocket, and must never be written to
-- Postgres; a table here would immediately be filled by every mouse move.

CREATE TABLE collab_documents (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- The Drive object this document is the collaborative state of. There is no
    -- foreign key yet because Drive (ROADMAP phase 1) does not exist; when it
    -- lands, resource_id becomes a reference to files(id) and this comment goes
    -- away. resource_type is the editor-registry key (§3b) and is validated by
    -- shape rather than by an enumeration: adding a fourth editor should not
    -- need a migration, and the value is never spliced into a subject, a key or
    -- a query.
    resource_type TEXT NOT NULL,
    resource_id   UUID NOT NULL,

    -- Log head. Only ever advanced by an append, under this row's lock.
    head_seq      BIGINT NOT NULL DEFAULT 0,
    -- Highest seq subsumed by a stored snapshot. 0 = never compacted.
    snapshot_seq  BIGINT NOT NULL DEFAULT 0,

    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- One collaborative document per object. The editor opens by resource, not
    -- by document id, so this is the lookup that must be unique.
    CONSTRAINT collab_documents_resource_key UNIQUE (resource_type, resource_id),
    CONSTRAINT collab_documents_resource_type_valid CHECK (resource_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    -- A snapshot can never claim to cover updates that do not exist yet.
    CONSTRAINT collab_documents_snapshot_bounded CHECK (snapshot_seq >= 0 AND snapshot_seq <= head_seq)
);

CREATE INDEX idx_collab_documents_workspace ON collab_documents (workspace_id);

-- The append-only log. Read path is always "everything after seq N for one
-- document", which the primary key serves directly.
CREATE TABLE collab_updates (
    document_id UUID   NOT NULL REFERENCES collab_documents(id) ON DELETE CASCADE,
    seq         BIGINT NOT NULL,
    -- Who produced it. Kept for audit and for attributing a corrupting client;
    -- ON DELETE SET NULL because deleting a user must not delete document
    -- history that other people's edits are interleaved with.
    actor_id    UUID   REFERENCES users(id) ON DELETE SET NULL,
    payload     BYTEA  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (document_id, seq),
    -- The hard ceiling on one update. The WebSocket path enforces a much lower
    -- bound (a keystroke batch is tens of bytes); this is the HTTP path's limit,
    -- which exists for a large paste or an offline client flushing a backlog.
    -- Without a bound one client can push the server's memory around.
    CONSTRAINT collab_updates_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 1048576)
);

-- Compacted state. `seq` is the log position the snapshot subsumes: loading a
-- document is this payload plus every collab_updates row with a greater seq.
CREATE TABLE collab_snapshots (
    document_id UUID   NOT NULL REFERENCES collab_documents(id) ON DELETE CASCADE,
    seq         BIGINT NOT NULL,
    actor_id    UUID   REFERENCES users(id) ON DELETE SET NULL,
    payload     BYTEA  NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (document_id, seq),
    CONSTRAINT collab_snapshots_payload_size CHECK (octet_length(payload) BETWEEN 1 AND 16777216)
);
