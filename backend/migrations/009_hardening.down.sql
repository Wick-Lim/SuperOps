-- Reverses 009_hardening.up.sql.
--
-- LOSSY BY CONSTRUCTION: 009 replaced three plaintext credential columns with
-- one-way SHA-256 hashes. A hash cannot be reversed, so rolling back restores
-- the columns but not their values. Consequences of a rollback:
--   * every session row is discarded (all users must log in again)
--   * every pending invitation is discarded (re-send them)
--   * every webhook is discarded (re-create them and redistribute the URLs)
-- Dropped dead tables (oauth_connections, user_preferences) are recreated
-- empty; they had no writers, so nothing is actually lost there.

-- G. Restore dead schema ----------------------------------------------------

CREATE TABLE IF NOT EXISTS oauth_connections (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    provider      TEXT NOT NULL,
    provider_uid  TEXT NOT NULL,
    access_token  TEXT,
    refresh_token TEXT,
    token_expiry  TIMESTAMPTZ,
    profile_data  JSONB NOT NULL DEFAULT '{}',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (provider, provider_uid)
);
CREATE INDEX IF NOT EXISTS idx_oauth_connections_user ON oauth_connections (user_id);

CREATE TABLE IF NOT EXISTS user_preferences (
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    sidebar_order       JSONB NOT NULL DEFAULT '[]',
    theme               TEXT NOT NULL DEFAULT 'system',
    notifications_email BOOLEAN NOT NULL DEFAULT TRUE,
    notifications_push  BOOLEAN NOT NULL DEFAULT TRUE,
    notifications_sound BOOLEAN NOT NULL DEFAULT TRUE,
    PRIMARY KEY (user_id, workspace_id)
);

-- F. updated_at triggers ----------------------------------------------------

DROP TRIGGER IF EXISTS trg_messages_updated_at   ON messages;
DROP TRIGGER IF EXISTS trg_channels_updated_at   ON channels;
DROP TRIGGER IF EXISTS trg_workspaces_updated_at ON workspaces;
DROP TRIGGER IF EXISTS trg_users_updated_at      ON users;
DROP FUNCTION IF EXISTS set_updated_at();

-- E. DM identity key --------------------------------------------------------

DROP INDEX IF EXISTS idx_channels_dm_key;
ALTER TABLE channels DROP COLUMN IF EXISTS dm_key;

-- D. Integrity constraints --------------------------------------------------

DROP INDEX IF EXISTS idx_invitations_expiry;
DROP INDEX IF EXISTS idx_invitations_pending_unique;

ALTER TABLE users         DROP CONSTRAINT IF EXISTS users_status_len;
ALTER TABLE custom_emojis DROP CONSTRAINT IF EXISTS custom_emojis_url_len;
ALTER TABLE custom_emojis DROP CONSTRAINT IF EXISTS custom_emojis_name_len;
ALTER TABLE reactions     DROP CONSTRAINT IF EXISTS reactions_emoji_len;
ALTER TABLE messages      DROP CONSTRAINT IF EXISTS messages_content_len;
ALTER TABLE user_blocks   DROP CONSTRAINT IF EXISTS user_blocks_no_self;

-- C. Index hygiene ----------------------------------------------------------

DROP INDEX IF EXISTS idx_custom_emojis_created_by;
DROP INDEX IF EXISTS idx_webhooks_created_by;
DROP INDEX IF EXISTS idx_webhooks_channel;
DROP INDEX IF EXISTS idx_invitations_invited_by;
DROP INDEX IF EXISTS idx_files_user;
DROP INDEX IF EXISTS idx_messages_pinned_by;
DROP INDEX IF EXISTS idx_channels_creator;
DROP INDEX IF EXISTS idx_workspaces_owner;
DROP INDEX IF EXISTS idx_user_blocks_blocked;
DROP INDEX IF EXISTS idx_bookmarks_message;
DROP INDEX IF EXISTS idx_users_fullname_trgm;
DROP INDEX IF EXISTS idx_users_username_trgm;

CREATE INDEX IF NOT EXISTS idx_invitations_email  ON invitations (email);
CREATE INDEX IF NOT EXISTS idx_audit_logs_actor   ON audit_logs (actor_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_files_workspace    ON files (workspace_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_user      ON messages (user_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_messages_parent    ON messages (parent_id, created_at ASC) WHERE parent_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_messages_channel_time ON messages (channel_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_reactions_message  ON reactions (message_id);
CREATE INDEX IF NOT EXISTS idx_workspaces_slug    ON workspaces (slug);
CREATE INDEX IF NOT EXISTS idx_users_username     ON users (username);
CREATE INDEX IF NOT EXISTS idx_users_email        ON users (email);

-- B. Pagination / hot-path indexes ------------------------------------------

DROP INDEX IF EXISTS idx_messages_retention;
DROP INDEX IF EXISTS idx_messages_scheduled_owner;
DROP INDEX IF EXISTS idx_messages_scheduled_due;
DROP INDEX IF EXISTS idx_messages_pinned;
DROP INDEX IF EXISTS idx_messages_thread_live;
DROP INDEX IF EXISTS idx_messages_channel_live;

-- A. Credential hashing -----------------------------------------------------

COMMENT ON COLUMN users.totp_secret IS NULL;

ALTER TABLE webhooks DROP CONSTRAINT IF EXISTS webhooks_channel_id_fkey;
ALTER TABLE webhooks ALTER COLUMN channel_id DROP NOT NULL;
ALTER TABLE webhooks
    ADD CONSTRAINT webhooks_channel_id_fkey
    FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE SET NULL;

DELETE FROM webhooks;
ALTER TABLE webhooks DROP CONSTRAINT IF EXISTS webhooks_token_hash_key;
ALTER TABLE webhooks DROP COLUMN IF EXISTS token_hash;
ALTER TABLE webhooks ADD COLUMN token TEXT NOT NULL UNIQUE;
CREATE INDEX IF NOT EXISTS idx_webhooks_token ON webhooks (token);

DELETE FROM invitations;
ALTER TABLE invitations DROP CONSTRAINT IF EXISTS invitations_token_hash_key;
ALTER TABLE invitations DROP COLUMN IF EXISTS token_hash;
ALTER TABLE invitations ADD COLUMN token TEXT NOT NULL UNIQUE;
CREATE INDEX IF NOT EXISTS idx_invitations_token ON invitations (token);

DELETE FROM sessions;
ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_refresh_token_hash_key;
ALTER TABLE sessions DROP COLUMN IF EXISTS refresh_token_hash;
ALTER TABLE sessions ADD COLUMN refresh_token TEXT NOT NULL UNIQUE;
CREATE INDEX IF NOT EXISTS idx_sessions_token ON sessions (refresh_token);
