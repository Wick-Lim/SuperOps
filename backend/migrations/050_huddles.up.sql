-- 050: Huddles — live audio and screen-share rooms attached to a channel.
--
-- THE DIVISION OF AUTHORITY IS THE WHOLE DESIGN. The SFU is authoritative for
-- PRESENCE IN THE CALL; this schema is authoritative for the huddle's
-- EXISTENCE and its AUTHORIZATION SCOPE. Participant rows are written only by
-- the webhook sink and the reconciler — never by a client saying "I joined",
-- which is a claim nothing can check.
--
-- A huddle is NOT an acl_object. Capability(subject, huddle) is defined as
-- Capability(subject, scope) and computed in the handler. Giving it a row would
-- make every huddle creation an ACL write that can disagree with the scope it
-- is derived from, and derivedTypes stays {workspace, channel, file}.

CREATE TABLE huddles (
    -- Minted in Go, because room_name is derived from it before the INSERT.
    id           UUID PRIMARY KEY,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- What the huddle hangs off. A shape CHECK with no enumeration and no FK,
    -- exactly as collab_documents.resource_type does, so an issue-scoped huddle
    -- is a row value rather than a migration. v1 registers only 'channel' in
    -- Go; an unknown scope_type is a 400, not a 500.
    scope_type TEXT NOT NULL,
    scope_id   UUID NOT NULL,

    -- Derived from THIS ROW, never from the scope: a re-started huddle must
    -- land in a fresh room or it inherits the state of the one that just ended.
    room_name TEXT NOT NULL,

    started_by UUID REFERENCES users(id) ON DELETE SET NULL,

    -- Stored so it cannot drift from the value handed to the SFU. The SFU is
    -- what actually enforces it.
    max_participants INT NOT NULL DEFAULT 10,

    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    ended_at     TIMESTAMPTZ,
    ended_reason TEXT,

    CONSTRAINT huddles_scope_type_valid CHECK (scope_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    CONSTRAINT huddles_room_name_key UNIQUE (room_name),
    CONSTRAINT huddles_max_participants_bounded CHECK (max_participants BETWEEN 2 AND 10),
    -- Ended is one fact in two columns; letting them disagree would make
    -- "is this huddle live" answerable two ways.
    CONSTRAINT huddles_ended_consistent CHECK ((ended_at IS NULL) = (ended_reason IS NULL)),
    CONSTRAINT huddles_ended_reason_valid CHECK (
        ended_reason IS NULL OR ended_reason IN
            ('empty', 'ended_by_user', 'reconciled', 'scope_deleted', 'failed'))
);

-- AT MOST ONE LIVE HUDDLE PER SCOPE, and this is the entire concurrency story
-- for "two people click Huddle at the same moment": the second INSERT conflicts
-- and the handler falls through to joining the first. Without it the two land
-- in different rooms and neither can hear the other — which looks like a
-- network fault and is a missing index.
--
-- The INSERT must name the predicate to infer this index:
--   ON CONFLICT (scope_type, scope_id) WHERE ended_at IS NULL DO NOTHING
CREATE UNIQUE INDEX uniq_huddle_live_scope
    ON huddles (scope_type, scope_id) WHERE ended_at IS NULL;

CREATE INDEX idx_huddles_live ON huddles (workspace_id) WHERE ended_at IS NULL;
CREATE INDEX idx_huddles_scope_time ON huddles (scope_type, scope_id, created_at DESC);

-- One row per SFU SESSION, not per user: the same person may rejoin, and the
-- participant sid is the identifier both sides can agree on. joined_at and
-- left_at are the SFU's timestamps, not ours — ours would describe when the
-- webhook arrived.
CREATE TABLE huddle_participants (
    huddle_id       UUID NOT NULL REFERENCES huddles(id) ON DELETE CASCADE,
    participant_sid TEXT NOT NULL,
    -- ON DELETE CASCADE is deliberate: deleting a user erases the durable
    -- record of who they met and for how long. Deactivation — the product's
    -- actual offboarding path — leaves the row intact.
    user_id UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    joined_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    left_at     TIMESTAMPTZ,
    left_reason TEXT,

    is_screen_sharing BOOLEAN NOT NULL DEFAULT FALSE,

    PRIMARY KEY (huddle_id, participant_sid),
    CONSTRAINT huddle_participants_left_consistent
        CHECK ((left_at IS NULL) = (left_reason IS NULL)),
    CONSTRAINT huddle_participants_left_reason_valid CHECK (
        left_reason IS NULL OR left_reason IN ('client', 'removed', 'room_ended', 'reconciled'))
);

-- "Who is in it right now" — the only hot read, and the only source the roster
-- frame is built from.
CREATE INDEX idx_huddle_participants_live
    ON huddle_participants (huddle_id) WHERE left_at IS NULL;
-- "What calls was this person in", for the offboarding surface.
CREATE INDEX idx_huddle_participants_user
    ON huddle_participants (user_id, joined_at DESC);

-- Webhook idempotency.
--
-- The SFU delivers at-least-once and retries. Without this a retried
-- participant_joined re-opens a session that already ended, and the roster
-- shows somebody who hung up ten minutes ago. The INSERT ... ON CONFLICT DO
-- NOTHING RETURNING runs in the SAME transaction as the state change, so
-- "recorded the event" and "applied the event" cannot come apart.
--
-- A dedupe window, not a log.
CREATE TABLE huddle_webhook_events (
    event_id    TEXT PRIMARY KEY,
    event_type  TEXT NOT NULL,
    received_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_huddle_webhook_events_received ON huddle_webhook_events (received_at);
