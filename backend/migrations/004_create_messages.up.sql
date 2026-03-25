CREATE TABLE messages (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    channel_id   UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id      UUID NOT NULL REFERENCES users(id),
    parent_id    UUID REFERENCES messages(id) ON DELETE SET NULL,
    content      TEXT NOT NULL,
    content_type TEXT NOT NULL DEFAULT 'markdown' CHECK (content_type IN ('markdown', 'system', 'file')),
    is_edited    BOOLEAN NOT NULL DEFAULT FALSE,
    is_deleted   BOOLEAN NOT NULL DEFAULT FALSE,
    reply_count  INT NOT NULL DEFAULT 0,
    metadata     JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_messages_channel_time ON messages (channel_id, created_at DESC);
CREATE INDEX idx_messages_parent ON messages (parent_id, created_at ASC) WHERE parent_id IS NOT NULL;
CREATE INDEX idx_messages_user ON messages (user_id, created_at DESC);

CREATE TABLE reactions (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    message_id UUID NOT NULL REFERENCES messages(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    emoji      TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (message_id, user_id, emoji)
);

CREATE INDEX idx_reactions_message ON reactions (message_id);

CREATE TABLE files (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    user_id       UUID NOT NULL REFERENCES users(id),
    message_id    UUID REFERENCES messages(id) ON DELETE SET NULL,
    name          TEXT NOT NULL,
    content_type  TEXT NOT NULL,
    size_bytes    BIGINT NOT NULL,
    storage_key   TEXT NOT NULL,
    thumbnail_key TEXT,
    width         INT,
    height        INT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_files_message ON files (message_id) WHERE message_id IS NOT NULL;
CREATE INDEX idx_files_workspace ON files (workspace_id, created_at DESC);
