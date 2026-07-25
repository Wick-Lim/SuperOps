-- 012: Push device tokens.
--
-- Until now a notification was a `notifications` row plus a WebSocket frame, so
-- a backgrounded or closed client was told nothing at all. This table is the
-- address book the worker fans push messages out to.
--
-- The token is the identity of the row, not the (user, token) pair. That is the
-- whole point of the UNIQUE constraint: a push token belongs to a *device*, and
-- devices are shared. When B signs in on A's phone the OS hands the app the
-- same token it gave A, and if the row for A survived, every message addressed
-- to A would ring on a phone that is now logged in as B. Registration is
-- therefore an upsert on `token` that REASSIGNS user_id (see
-- user.Repository.RegisterDevice), which is only expressible because the token
-- is unique on its own.
--
-- last_seen_at is refreshed on every re-registration. The client registers on
-- every launch, so a token that stops being refreshed is a device that stopped
-- launching the app; nothing prunes on it today, but it is the only signal that
-- would let something to.

CREATE TABLE device_tokens (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    token        TEXT NOT NULL,
    platform     TEXT NOT NULL DEFAULT 'unknown',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT device_tokens_token_key UNIQUE (token),
    -- Expo push tokens are ~40 characters; the bound exists so a client cannot
    -- park an arbitrary blob in the table, not because the format is known.
    CONSTRAINT device_tokens_token_len CHECK (char_length(token) BETWEEN 1 AND 512),
    CONSTRAINT device_tokens_platform_valid CHECK (platform IN ('ios', 'android', 'web', 'unknown'))
);

-- The only read path is "every token for this user", issued once per push
-- recipient in the worker's fan-out.
CREATE INDEX idx_device_tokens_user ON device_tokens (user_id);
