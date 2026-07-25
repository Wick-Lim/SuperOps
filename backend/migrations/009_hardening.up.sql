-- 009: Security, integrity and performance hardening.
--
-- Groups:
--   A. Credential hashing (sessions / invitations / webhooks)
--   B. Pagination tiebreaker + hot-path indexes
--   C. Index hygiene (drop redundant/unused, add missing FK-child indexes)
--   D. Integrity constraints
--   E. DM identity key (race-proof 1:1 idempotency)
--   F. updated_at triggers
--   G. Drop dead schema carrying plaintext credentials
--
-- golang-migrate wraps this file in a single implicit transaction, so no
-- CREATE INDEX CONCURRENTLY / ALTER TYPE ADD VALUE here.

-- ---------------------------------------------------------------------------
-- A. Credential hashing
--
-- All three tokens are 24-32 bytes of crypto/rand, so a single SHA-256 is the
-- right primitive (unlike passwords, they need no key stretching and must stay
-- cheap enough for an indexed equality lookup). Existing rows are backfilled
-- from the plaintext, so live sessions, pending invites and deployed webhook
-- URLs keep working across the upgrade.
-- ---------------------------------------------------------------------------

ALTER TABLE sessions ADD COLUMN refresh_token_hash TEXT;
UPDATE sessions SET refresh_token_hash = encode(digest(refresh_token, 'sha256'), 'hex');
ALTER TABLE sessions ALTER COLUMN refresh_token_hash SET NOT NULL;
DROP INDEX IF EXISTS idx_sessions_token;
ALTER TABLE sessions DROP COLUMN refresh_token;
ALTER TABLE sessions ADD CONSTRAINT sessions_refresh_token_hash_key UNIQUE (refresh_token_hash);

ALTER TABLE invitations ADD COLUMN token_hash TEXT;
UPDATE invitations SET token_hash = encode(digest(token, 'sha256'), 'hex');
ALTER TABLE invitations ALTER COLUMN token_hash SET NOT NULL;
DROP INDEX IF EXISTS idx_invitations_token;
ALTER TABLE invitations DROP COLUMN token;
ALTER TABLE invitations ADD CONSTRAINT invitations_token_hash_key UNIQUE (token_hash);

ALTER TABLE webhooks ADD COLUMN token_hash TEXT;
UPDATE webhooks SET token_hash = encode(digest(token, 'sha256'), 'hex');
ALTER TABLE webhooks ALTER COLUMN token_hash SET NOT NULL;
DROP INDEX IF EXISTS idx_webhooks_token;
ALTER TABLE webhooks DROP COLUMN token;
ALTER TABLE webhooks ADD CONSTRAINT webhooks_token_hash_key UNIQUE (token_hash);

-- An incoming webhook whose channel was deleted posts nowhere; delete it with
-- the channel instead of leaving a live token pointing at NULL.
ALTER TABLE webhooks DROP CONSTRAINT IF EXISTS webhooks_channel_id_fkey;
DELETE FROM webhooks WHERE channel_id IS NULL;
ALTER TABLE webhooks ALTER COLUMN channel_id SET NOT NULL;
ALTER TABLE webhooks
    ADD CONSTRAINT webhooks_channel_id_fkey
    FOREIGN KEY (channel_id) REFERENCES channels(id) ON DELETE CASCADE;

-- totp_secret is a shared secret, not a hash; it cannot be one-way hashed and
-- still verify codes. Flag it so operators know it needs at-rest encryption.
COMMENT ON COLUMN users.totp_secret IS
    'Base32 TOTP shared secret. Stored plaintext by necessity (verification requires the secret). Protect with database-level at-rest encryption.';

-- ---------------------------------------------------------------------------
-- B. Pagination tiebreaker + hot-path indexes
--
-- Cursors previously encoded created_at alone, so any two rows sharing a
-- timestamp (routinely produced by the scheduled-message batch UPDATE) were
-- silently skipped at a page boundary. Ordering is now (created_at, id), which
-- is total. These indexes match the new ORDER BY and the WHERE clauses that
-- the old (channel_id, created_at) index could not cover.
-- ---------------------------------------------------------------------------

CREATE INDEX idx_messages_channel_live
    ON messages (channel_id, created_at DESC, id DESC)
    WHERE parent_id IS NULL AND is_deleted = FALSE AND is_scheduled = FALSE;

CREATE INDEX idx_messages_thread_live
    ON messages (parent_id, created_at ASC, id ASC)
    WHERE parent_id IS NOT NULL AND is_deleted = FALSE AND is_scheduled = FALSE;

CREATE INDEX idx_messages_pinned
    ON messages (channel_id, pinned_at DESC)
    WHERE is_pinned = TRUE AND is_deleted = FALSE AND is_scheduled = FALSE;

-- The 30s worker poll ran an unindexed sequential scan of the largest table in
-- the system, forever, whether or not any scheduled message existed.
CREATE INDEX idx_messages_scheduled_due
    ON messages (scheduled_at)
    WHERE is_scheduled = TRUE;

CREATE INDEX idx_messages_scheduled_owner
    ON messages (user_id, channel_id, scheduled_at)
    WHERE is_scheduled = TRUE;

-- Retention purge: WHERE channel_id = ANY(...) AND created_at < cutoff.
CREATE INDEX idx_messages_retention
    ON messages (channel_id, created_at);

-- ---------------------------------------------------------------------------
-- C. Index hygiene
-- ---------------------------------------------------------------------------

-- Duplicates of the index Postgres already builds for a UNIQUE constraint:
-- pure write amplification for zero read benefit.
DROP INDEX IF EXISTS idx_users_email;        -- users_email_key
DROP INDEX IF EXISTS idx_users_username;     -- users_username_key
DROP INDEX IF EXISTS idx_workspaces_slug;    -- workspaces_slug_key
DROP INDEX IF EXISTS idx_reactions_message;  -- leading col of UNIQUE(message_id,user_id,emoji)

-- Superseded by the partial indexes above.
DROP INDEX IF EXISTS idx_messages_channel_time;
DROP INDEX IF EXISTS idx_messages_parent;

-- No query in the codebase can use these.
DROP INDEX IF EXISTS idx_messages_user;
DROP INDEX IF EXISTS idx_files_workspace;
DROP INDEX IF EXISTS idx_audit_logs_actor;
DROP INDEX IF EXISTS idx_invitations_email;

-- Trigram support for user search. pg_trgm was installed in 000 and never used
-- while user search ran unindexed ILIKE '%q%' across the whole users table.
CREATE INDEX idx_users_username_trgm ON users USING gin (username gin_trgm_ops);
CREATE INDEX idx_users_fullname_trgm ON users USING gin (full_name gin_trgm_ops);

-- FK child columns with no index force a sequential scan of the child table on
-- every parent DELETE. workspace.Repository.Delete is a live code path.
CREATE INDEX idx_bookmarks_message ON bookmarks (message_id);
CREATE INDEX idx_user_blocks_blocked ON user_blocks (blocked_id);
CREATE INDEX idx_workspaces_owner ON workspaces (owner_id);
CREATE INDEX idx_channels_creator ON channels (creator_id) WHERE creator_id IS NOT NULL;
CREATE INDEX idx_messages_pinned_by ON messages (pinned_by) WHERE pinned_by IS NOT NULL;
CREATE INDEX idx_files_user ON files (user_id);
CREATE INDEX idx_invitations_invited_by ON invitations (invited_by);
CREATE INDEX idx_webhooks_channel ON webhooks (channel_id);
CREATE INDEX idx_webhooks_created_by ON webhooks (created_by);
CREATE INDEX idx_custom_emojis_created_by ON custom_emojis (created_by);

-- ---------------------------------------------------------------------------
-- D. Integrity constraints
--
-- Length caps are added NOT VALID: they are enforced for every new INSERT and
-- UPDATE (which is the point) without forcing a full table scan of existing
-- rows at migration time.
-- ---------------------------------------------------------------------------

ALTER TABLE user_blocks
    ADD CONSTRAINT user_blocks_no_self CHECK (blocker_id <> blocked_id) NOT VALID;

ALTER TABLE messages
    ADD CONSTRAINT messages_content_len CHECK (char_length(content) <= 40000) NOT VALID;

ALTER TABLE reactions
    ADD CONSTRAINT reactions_emoji_len CHECK (char_length(emoji) BETWEEN 1 AND 64) NOT VALID;

ALTER TABLE custom_emojis
    ADD CONSTRAINT custom_emojis_name_len CHECK (char_length(name) BETWEEN 1 AND 64) NOT VALID;

ALTER TABLE custom_emojis
    ADD CONSTRAINT custom_emojis_url_len CHECK (char_length(image_url) <= 2048) NOT VALID;

ALTER TABLE users
    ADD CONSTRAINT users_status_len CHECK (
        char_length(status_text) <= 100 AND char_length(status_emoji) <= 64
    ) NOT VALID;

-- At most one live invitation per (workspace, email). Older duplicates are
-- retired first so the unique index can be created.
UPDATE invitations i
SET status = 'expired'
WHERE i.status = 'pending'
  AND EXISTS (
      SELECT 1 FROM invitations j
      WHERE j.status = 'pending'
        AND j.workspace_id = i.workspace_id
        AND lower(j.email) = lower(i.email)
        AND (j.created_at, j.id) > (i.created_at, i.id)
  );

CREATE UNIQUE INDEX idx_invitations_pending_unique
    ON invitations (workspace_id, lower(email))
    WHERE status = 'pending';

-- Worker sweeps expired invitations; this is the predicate it filters on.
CREATE INDEX idx_invitations_expiry ON invitations (status, expires_at);

-- ---------------------------------------------------------------------------
-- E. DM identity key
--
-- 1:1 DM idempotency was a read-then-write (FindDirectChannel) with no lock and
-- no constraint, so two concurrent requests both missed and both inserted.
-- dm_key is the sorted participant pair; a partial unique index makes the race
-- impossible and lets CreateDM use ON CONFLICT.
-- ---------------------------------------------------------------------------

ALTER TABLE channels ADD COLUMN dm_key TEXT;

WITH pairs AS (
    SELECT cm.channel_id,
           MIN(cm.user_id::text) || ':' || MAX(cm.user_id::text) AS k,
           ch.workspace_id,
           ch.created_at
    FROM channel_members cm
    JOIN channels ch ON ch.id = cm.channel_id
    WHERE ch.type = 'dm'
    GROUP BY cm.channel_id, ch.workspace_id, ch.created_at
    HAVING COUNT(*) = 2
), ranked AS (
    SELECT channel_id, k,
           ROW_NUMBER() OVER (PARTITION BY workspace_id, k ORDER BY created_at, channel_id) AS rn
    FROM pairs
)
UPDATE channels c
SET dm_key = r.k
FROM ranked r
WHERE c.id = r.channel_id AND r.rn = 1;

CREATE UNIQUE INDEX idx_channels_dm_key
    ON channels (workspace_id, dm_key)
    WHERE dm_key IS NOT NULL;

-- ---------------------------------------------------------------------------
-- F. updated_at triggers
--
-- Every updated_at column relied on the application writing NOW() by hand, and
-- several write paths simply did not, leaving the column unusable for change
-- tracking or cache invalidation.
-- ---------------------------------------------------------------------------

CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at = NOW();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

CREATE TRIGGER trg_users_updated_at      BEFORE UPDATE ON users      FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_workspaces_updated_at BEFORE UPDATE ON workspaces FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_channels_updated_at   BEFORE UPDATE ON channels   FOR EACH ROW EXECUTE FUNCTION set_updated_at();
CREATE TRIGGER trg_messages_updated_at   BEFORE UPDATE ON messages   FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- G. Drop dead schema
--
-- Neither table is referenced by a single line of Go (verified by grep across
-- backend/). oauth_connections additionally carries plaintext access_token and
-- refresh_token columns — a liability with no compensating use.
-- ---------------------------------------------------------------------------

DROP TABLE IF EXISTS oauth_connections;
DROP TABLE IF EXISTS user_preferences;
