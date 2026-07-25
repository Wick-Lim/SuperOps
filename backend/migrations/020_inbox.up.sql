-- 020: the unified inbox — one "what needs my attention" list for nine pillars.
--
-- See docs/plans/01-phase0-remainder.md Part A, and docs/plans/README.md
-- "Resolved conflicts" §1: this schema REPLACES `notifications` as the product's
-- notification substrate. Pillars add no enum values and no columns to any table
-- here; they publish `superops.{ws}.inbox.requested` with a `kind` string and an
-- explicit recipient list, and a single durable consumer writes these rows.
--
-- Four decisions are load-bearing and are why the DDL looks like this.
--
-- 1. TWO tables, and the reason is idempotency rather than taste. Coalescing
--    means `unread_count = unread_count + 1`, and an increment is NOT idempotent
--    under the at-least-once delivery every consumer in this tree has. The event
--    table restores the property: `INSERT ... ON CONFLICT (id) DO NOTHING` on
--    inbox_events is the gate, and ONLY an insert that actually happened is
--    allowed to move the counter — in the same transaction. This is the same
--    mechanism `notifications` already relies on (internal/notification's
--    notificationID + Repository.Create), carried forward rather than reinvented.
--
-- 2. `kind` is TEXT + CHECK, not an enum. Migration 005 made notification_type
--    an ENUM with five message-shaped values; nine pillars would mean an
--    ALTER TYPE per pillar, and four separate plans independently tried to do
--    exactly that (README §1). A regex CHECK of the same shape internal/audit
--    already uses for actions ('<resource>.<verb>') costs nothing and needs no
--    migration to extend.
--
-- 3. Ids are derived UUIDv5, in the SAME namespace and with the SAME
--    length-prefixed key encoding as internal/notification's notificationID.
--    Both are load-bearing: the namespace because changing it duplicates every
--    notification exactly once on the first fan-out after the change, and the
--    length prefixing because no choice of separator can then make two different
--    tuples collide. The backfill below derives ids the same way (via
--    uuid_generate_v5) so that a message.created event still sitting in
--    JetStream at cutover collapses onto its backfilled row instead of counting
--    twice.
--
-- 4. An item is keyed on (user, SUBJECT), not on the object. The subject is what
--    a burst coalesces under — the channel, the doc, the issue — which is what
--    turns forty mentions in #alerts into one row that says "40". `id` is the
--    derived UUIDv5 of that triple rather than a random uuid, so it is stable
--    and usable as the tiebreaker httputil.Cursor requires.
--
-- What is NOT here, deliberately: no watch/subscribe table (recipients in v1 are
-- directed, so the fan-out audience is an explicit list rather than a query), and
-- no inbox item for an undirected channel message — that is the channel unread
-- badge's job and it already works. See "the hard part" in the plan: the two
-- systems must cover DISJOINT things and agree at their single overlap, which is
-- PUT /channels/{id}/read.
--
-- `notifications` is untouched. The old table keeps its rows and the four old
-- routes keep working, re-pointed at a projection of inbox_items; it is dropped
-- in a later migration once the shipped client has moved.

-- The event. Immutable, one row per (thing that happened, person it happened to).
CREATE TABLE inbox_events (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    kind         TEXT NOT NULL
                 CHECK (kind ~ '^[a-z][a-z0-9_]{0,23}\.[a-z][a-z0-9_]{0,23}$'),

    -- What it is about: the message, the comment, the issue.
    object_type  TEXT NOT NULL CHECK (object_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    object_id    UUID NOT NULL,

    -- What it coalesces under: the channel, the doc, the issue.
    subject_type TEXT NOT NULL CHECK (subject_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    subject_id   UUID NOT NULL,

    actor_id     UUID REFERENCES users(id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    -- Written by every pillar, so it is capped at the Deliver boundary (4 KB
    -- serialized) rather than being allowed to become an unbounded content store.
    data         JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The only read of this table is "the events behind one inbox item, newest
-- first" — and the reconciler's recount, which uses the same prefix.
CREATE INDEX idx_inbox_events_item
    ON inbox_events (user_id, subject_type, subject_id, created_at DESC, id DESC);
-- Retention sweep.
CREATE INDEX idx_inbox_events_age ON inbox_events (created_at);

-- The item. Mutable, one row per (person, subject). This is what the inbox lists.
CREATE TABLE inbox_items (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    subject_type TEXT NOT NULL,
    subject_id   UUID NOT NULL,

    state        TEXT NOT NULL DEFAULT 'unread'
                 CHECK (state IN ('unread','read','done')),
    -- The highest-priority reason this item is here; drives the icon and the
    -- kind= filter. Ordered by inbox.kindRank in Go, not by the database — a
    -- ranking in a CHECK constraint would need a migration per pillar, which is
    -- the mistake decision 2 exists to avoid.
    top_kind     TEXT NOT NULL,

    unread_count  INT  NOT NULL DEFAULT 0 CHECK (unread_count >= 0),
    event_count   INT  NOT NULL DEFAULT 0 CHECK (event_count  >= 0),
    last_event_id UUID NOT NULL,
    last_actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    preview      TEXT NOT NULL DEFAULT '',

    first_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at      TIMESTAMPTZ,
    done_at      TIMESTAMPTZ,
    -- Last time this item was carried by a digest. NULL = mailable.
    notified_at  TIMESTAMPTZ,

    UNIQUE (user_id, subject_type, subject_id)
);

-- The list query: open items for one user, newest activity first.
CREATE INDEX idx_inbox_items_open
    ON inbox_items (user_id, last_at DESC, id DESC) WHERE state <> 'done';
-- The badge, and the digest candidate scan.
CREATE INDEX idx_inbox_items_unread
    ON inbox_items (user_id, last_at DESC, id DESC) WHERE state = 'unread';
-- Workspace-filtered list.
CREATE INDEX idx_inbox_items_ws
    ON inbox_items (user_id, workspace_id, last_at DESC, id DESC) WHERE state <> 'done';
-- Digest sweep: unread, never mailed, quiet long enough. Partial so it stays small.
CREATE INDEX idx_inbox_items_digest
    ON inbox_items (workspace_id, user_id, last_at) WHERE state = 'unread' AND notified_at IS NULL;
-- The one overlap with the channel unread badge: PUT /channels/{id}/read marks
-- this channel's items read in the same transaction as the read marker.
CREATE INDEX idx_inbox_items_subject
    ON inbox_items (subject_type, subject_id, user_id);
-- The reconciler walks users in id order, a slice per run.
CREATE INDEX idx_inbox_items_user ON inbox_items (user_id, id);

-- Per-type delivery preference. Resolution is exact kind > '<resource>.*' > '*',
-- and it is evaluated in exactly one function (inbox.Prefs.Resolve) against a map
-- loaded with ONE query for the whole recipient set. No per-recipient preference
-- query is permitted anywhere in the fan-out.
CREATE TABLE notification_prefs (
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL
                 CHECK (kind ~ '^(\*|[a-z][a-z0-9_]{0,23}\.(\*|[a-z][a-z0-9_]{0,23}))$'),
    in_app       BOOLEAN NOT NULL DEFAULT TRUE,
    push         BOOLEAN NOT NULL DEFAULT TRUE,
    email        TEXT    NOT NULL DEFAULT 'digest'
                 CHECK (email IN ('never','digest','immediate')),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, workspace_id, kind)
);

-- Rate ceiling on digests, so a pathological workspace cannot mail hourly forever.
CREATE TABLE inbox_digest_state (
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    last_sent_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);

-- --------------------------------------------------------------------------
-- Backfill.
--
-- Unread `notifications` rows become items and events; read rows are left
-- behind, because the only thing the old table is still for is the transition.
--
-- Two properties this backfill MUST have, or the first deploy is an incident:
--
--   * workspace_id is resolved by joining data->>'channel_id' to channels. A row
--     whose channel is gone is SKIPPED, not defaulted — inventing a workspace for
--     it would put someone else's notification in someone's inbox.
--   * notified_at is set to NOW() on every backfilled row, so the first digest
--     cycle after deploy sees an empty candidate set instead of mailing the
--     entire company its notification history. Dropping this line is the
--     difference between a quiet cutover and every user receiving a digest of
--     everything they ever ignored, ten minutes later.
--
-- The `system` type carried reactions, whose notification id mixed in the
-- reactor and the emoji; neither survives in `data`, so backfilled reaction
-- events key on the message alone. Two different people's reactions to one
-- message therefore collapse into a single backfilled event. That is a
-- one-time, backwards-only imprecision on rows the user has already not read.
-- --------------------------------------------------------------------------

CREATE TEMPORARY TABLE inbox_backfill AS
SELECT
    n.id                                       AS legacy_id,
    n.user_id::text                            AS user_id,
    c.workspace_id::text                       AS workspace_id,
    CASE n.type
        WHEN 'mention'       THEN 'message.mention'
        WHEN 'dm'            THEN 'message.dm'
        WHEN 'thread_reply'  THEN 'message.thread_reply'
        WHEN 'channel_invite' THEN 'channel.invited'
        ELSE 'message.reaction'
    END                                        AS kind,
    CASE WHEN n.type = 'channel_invite' THEN 'channel' ELSE 'message' END AS object_type,
    CASE WHEN n.type = 'channel_invite'
         THEN (n.data->>'channel_id')
         ELSE (n.data->>'message_id')
    END                                        AS object_id,
    'channel'                                  AS subject_type,
    (n.data->>'channel_id')                    AS subject_id,
    n.title                                    AS title,
    n.body                                     AS body,
    n.data                                     AS data,
    n.created_at                               AS created_at,
    CASE n.type
        WHEN 'dm'             THEN 50
        WHEN 'mention'        THEN 40
        WHEN 'channel_invite' THEN 30
        WHEN 'thread_reply'   THEN 20
        ELSE 10
    END                                        AS rank
  FROM notifications n
  JOIN channels c ON c.id = (n.data->>'channel_id')::uuid
 WHERE n.is_read = FALSE
   AND n.data ? 'channel_id'
   AND (n.type = 'channel_invite' OR n.data ? 'message_id');

-- Items first: one per (user, channel), carrying the burst's count and the
-- highest-ranked kind in it.
INSERT INTO inbox_items (
    id, user_id, workspace_id, subject_type, subject_id, state, top_kind,
    unread_count, event_count, last_event_id, last_actor_id, title, preview,
    first_at, last_at, notified_at
)
SELECT
    uuid_generate_v5(
        '3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41'::uuid,
        format('%s:%s|%s:%s|%s:%s',
               length(b.user_id), b.user_id,
               length('channel'), 'channel',
               length(b.subject_id), b.subject_id)),
    b.user_id::uuid,
    b.workspace_id::uuid,
    'channel',
    b.subject_id::uuid,
    'unread',
    (ARRAY_AGG(b.kind ORDER BY b.rank DESC, b.created_at DESC))[1],
    COUNT(*),
    COUNT(*),
    -- The newest event's derived id; recomputed identically below.
    (ARRAY_AGG(
        uuid_generate_v5(
            '3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41'::uuid,
            format('%s:%s|%s:%s|%s:%s',
                   length(b.kind), b.kind,
                   length(b.user_id), b.user_id,
                   length(b.object_id), b.object_id))
        ORDER BY b.created_at DESC, b.legacy_id DESC))[1],
    NULL,
    (ARRAY_AGG(b.title ORDER BY b.created_at DESC, b.legacy_id DESC))[1],
    LEFT((ARRAY_AGG(b.body ORDER BY b.created_at DESC, b.legacy_id DESC))[1], 140),
    MIN(b.created_at),
    MAX(b.created_at),
    NOW()
  FROM inbox_backfill b
 GROUP BY b.user_id, b.workspace_id, b.subject_id
ON CONFLICT (user_id, subject_type, subject_id) DO NOTHING;

-- Then the events behind them, with ids derived exactly as inbox.EventID does.
INSERT INTO inbox_events (
    id, user_id, workspace_id, kind, object_type, object_id,
    subject_type, subject_id, actor_id, title, body, data, created_at
)
SELECT DISTINCT ON (event_id)
    event_id,
    b.user_id::uuid,
    b.workspace_id::uuid,
    b.kind,
    b.object_type,
    b.object_id::uuid,
    'channel',
    b.subject_id::uuid,
    NULL,
    b.title,
    b.body,
    b.data,
    b.created_at
  FROM inbox_backfill b,
       LATERAL (
           SELECT uuid_generate_v5(
               '3d0f5e2a-6c1b-4f7e-9a1d-2b8c5f0e7a41'::uuid,
               format('%s:%s|%s:%s|%s:%s',
                      length(b.kind), b.kind,
                      length(b.user_id), b.user_id,
                      length(b.object_id), b.object_id)) AS event_id
       ) e
 ORDER BY event_id, b.created_at DESC
ON CONFLICT (id) DO NOTHING;

-- The counts above were taken before the DISTINCT above collapsed any duplicate
-- derived ids (only reachable for backfilled reactions). Bring them back into
-- agreement so inbox_reconcile does not report the backfill itself as drift on
-- its very first run.
UPDATE inbox_items i
   SET unread_count = c.n,
       event_count  = c.n
  FROM (
      SELECT e.user_id, e.subject_type, e.subject_id, COUNT(*) AS n
        FROM inbox_events e
       GROUP BY e.user_id, e.subject_type, e.subject_id
  ) c
 WHERE i.user_id = c.user_id
   AND i.subject_type = c.subject_type
   AND i.subject_id = c.subject_id
   AND i.unread_count <> c.n;

DROP TABLE inbox_backfill;
