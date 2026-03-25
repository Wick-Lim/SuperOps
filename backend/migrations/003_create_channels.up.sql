CREATE TYPE channel_type AS ENUM ('public', 'private', 'dm', 'group_dm');

CREATE TABLE channels (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            TEXT,
    slug            TEXT,
    description     TEXT NOT NULL DEFAULT '',
    type            channel_type NOT NULL DEFAULT 'public',
    topic           TEXT NOT NULL DEFAULT '',
    is_archived     BOOLEAN NOT NULL DEFAULT FALSE,
    creator_id      UUID REFERENCES users(id),
    last_message_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, slug)
);

CREATE INDEX idx_channels_workspace ON channels (workspace_id, type);
CREATE INDEX idx_channels_last_message ON channels (workspace_id, last_message_at DESC NULLS LAST);

CREATE TABLE channel_members (
    channel_id        UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    user_id           UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role              TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin', 'member')),
    last_read_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    muted             BOOLEAN NOT NULL DEFAULT FALSE,
    notification_pref TEXT NOT NULL DEFAULT 'default' CHECK (notification_pref IN ('all', 'mentions', 'none', 'default')),
    joined_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (channel_id, user_id)
);

CREATE INDEX idx_channel_members_user ON channel_members (user_id);
