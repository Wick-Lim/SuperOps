-- 021: audit coverage — partitioning, coalescing, and a per-workspace hash chain.
--
-- See docs/plans/01-phase0-remainder.md Part B. Five changes to `audit_logs`,
-- one new table, and a conversion of the table itself to monthly range
-- partitions. Each is here for a specific failure the current schema has.
--
-- 1. resource_id becomes TEXT. It is UUID today, and internal/admin/mail.go
--    passed the transport name ("smtp") into it — an INSERT that fails with
--    22P02 every time, whose error was discarded, so `mail.test_sent` had never
--    once been recorded. The Go side of that is already fixed (the transport
--    moved into metadata); this makes the column able to hold the non-uuid
--    identifiers the remaining categories genuinely have (a storage key, an
--    export filter, a transport name).
--
-- 2. Coalescing. dedupe_key is a derived UUIDv5 over
--    (actor, action, resource_type, resource_id, hour) — the same technique as
--    internal/notification's notificationID, and the same technique migration 020
--    uses. Fifty downloads of one file in an afternoon become one row with
--    event_count = 50 and last_at advanced, which is all an auditor wanted from
--    them. NULL for anything that must never coalesce: every mutation, every
--    authentication event.
--
-- 3. A per-workspace hash chain, so an in-place edit or a deletion is
--    DETECTABLE. Per workspace and not global, because a global chain serialises
--    every audit insert in the deployment.
--
--    Read the honest version before relying on it: a chain that lives in the
--    same database as the thing it protects, guarded by an administrator who has
--    psql, is theatre — anyone with UPDATE can recompute the whole chain. It
--    becomes real only at the moment the head is anchored somewhere that
--    administrator does not control, which is why internal/audit's Sink is not
--    an optional extra and why AUDIT_SINK's default ('log', into a pipeline that
--    in most deployments already ships off-box) has to be useful rather than a
--    placeholder. anchored_seq below is the record of how far that has got.
--
--    Chained and coalesced are mutually exclusive, enforced by a CHECK: a
--    coalesced row is mutated on every repeat, so a hash over it would go stale
--    on the second event. The chain therefore covers exactly the immutable rows
--    — authentication, authorization changes, sharing, configuration, and any
--    egress event whose call site did not ask to coalesce.
--
-- 4. Query indexes with the workspace as the LEADING column. 009_hardening
--    dropped idx_audit_logs_actor as unusable, correctly: every query against
--    this table is workspace-scoped first (internal/admin's h.scope), so an
--    actor-leading index was never reachable.
--
-- 5. Monthly RANGE partitions on created_at, so retention is a partition DROP
--    rather than the batched, capped, advisory-locked DELETE that cmd/worker's
--    runRetention has to be. That DELETE exists because an unbounded one on a
--    large table was a production problem; partitioning means audit never has
--    that problem at all.
--
--    The conversion is the cheap one: add a CHECK that implies the bound, rename
--    the existing table into place as the oldest partition, create the
--    partitioned parent, ATTACH. No data moves. The locks are ACCESS EXCLUSIVE
--    for the rename and the attach, and the CHECK validation is one sequential
--    scan — seconds on any realistic table. If that scan is ever too slow, the
--    fallback is create-alongside, copy in batches, swap; it is not taken here
--    because it costs a release of double-writing.
--
--    The oldest partition is named for the month this migration RUNS in and its
--    lower bound is MINVALUE, so it swallows all pre-existing history. That
--    naming matters: cmd/worker's audit_partitions job creates
--    `audit_logs_pYYYY_MM` with CREATE TABLE IF NOT EXISTS, so it finds this one
--    by name and skips it instead of failing on an overlapping range.
--
-- Deliberately NO GIN index on metadata. No query needs it and it is the most
-- expensive index this table could carry.
--
-- Deliberately NOT pg_partman: a new extension in the Compose and Helm images to
-- replace sixty lines of Go inside a job loop this worker already has.

-- --- 1. resource_id --------------------------------------------------------
ALTER TABLE audit_logs ALTER COLUMN resource_id TYPE TEXT USING resource_id::text;

-- --- 2/3. new columns ------------------------------------------------------
ALTER TABLE audit_logs ADD COLUMN dedupe_key  UUID;
ALTER TABLE audit_logs ADD COLUMN event_count INT NOT NULL DEFAULT 1;
ALTER TABLE audit_logs ADD COLUMN last_at     TIMESTAMPTZ;
ALTER TABLE audit_logs ADD COLUMN chain_seq   BIGINT;
ALTER TABLE audit_logs ADD COLUMN prev_hash   BYTEA;
ALTER TABLE audit_logs ADD COLUMN hash        BYTEA;
ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_chain_or_dedupe
    CHECK (dedupe_key IS NULL OR chain_seq IS NULL);

-- The partition key has to be part of every unique key on a partitioned table,
-- so the primary key gains created_at. Nothing references audit_logs, and the
-- one query shape (keyset over (created_at, id)) is unaffected.
ALTER TABLE audit_logs DROP CONSTRAINT audit_logs_pkey;
ALTER TABLE audit_logs ADD PRIMARY KEY (id, created_at);

-- --- 5. partition conversion ----------------------------------------------
DO $$
DECLARE
    next_month DATE := (date_trunc('month', NOW()) + INTERVAL '1 month')::date;
    legacy     TEXT := 'audit_logs_p' || to_char(NOW(), 'YYYY_MM');
BEGIN
    -- Implies the partition bound, so ATTACH does not have to re-scan for it.
    EXECUTE format(
        'ALTER TABLE audit_logs ADD CONSTRAINT audit_logs_legacy_bound CHECK (created_at < %L)',
        next_month);

    EXECUTE format('ALTER TABLE audit_logs RENAME TO %I', legacy);

    CREATE TABLE audit_logs (
        id            UUID NOT NULL DEFAULT uuid_generate_v4(),
        workspace_id  UUID REFERENCES workspaces(id) ON DELETE SET NULL,
        actor_id      UUID REFERENCES users(id) ON DELETE SET NULL,
        action        TEXT NOT NULL,
        resource_type TEXT NOT NULL,
        resource_id   TEXT,
        metadata      JSONB NOT NULL DEFAULT '{}',
        ip_address    INET,
        created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
        dedupe_key    UUID,
        event_count   INT NOT NULL DEFAULT 1,
        last_at       TIMESTAMPTZ,
        chain_seq     BIGINT,
        prev_hash     BYTEA,
        hash          BYTEA,
        PRIMARY KEY (id, created_at),
        CONSTRAINT audit_logs_chain_or_dedupe CHECK (dedupe_key IS NULL OR chain_seq IS NULL)
    ) PARTITION BY RANGE (created_at);

    EXECUTE format(
        'ALTER TABLE audit_logs ATTACH PARTITION %I FOR VALUES FROM (MINVALUE) TO (%L)',
        legacy, next_month);

    -- The CHECK has done its job; leaving it makes the partition carry a
    -- redundant constraint that a future DETACH would have to reason about.
    EXECUTE format('ALTER TABLE %I DROP CONSTRAINT audit_logs_legacy_bound', legacy);

    -- Two months of lead time from the moment this lands, so a worker that never
    -- starts still has until then before an INSERT fails. cmd/worker's
    -- audit_partitions job maintains the window from here.
    EXECUTE format(
        'CREATE TABLE audit_logs_p%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
        to_char(next_month, 'YYYY_MM'), next_month, (next_month + INTERVAL '1 month')::date);
    EXECUTE format(
        'CREATE TABLE audit_logs_p%s PARTITION OF audit_logs FOR VALUES FROM (%L) TO (%L)',
        to_char(next_month + INTERVAL '1 month', 'YYYY_MM'),
        (next_month + INTERVAL '1 month')::date,
        (next_month + INTERVAL '2 month')::date);
END
$$;

-- --- 2. coalescing index ---------------------------------------------------
-- Partial, so the overwhelming majority of rows (which never coalesce) carry no
-- entry at all. Includes created_at because a unique index on a partitioned
-- table must contain the partition key — which is harmless here, because
-- dedupe_key already encodes an hour bucket and coalescable rows have their
-- created_at pinned to the start of that bucket. See audit.dedupeKey.
CREATE UNIQUE INDEX idx_audit_logs_dedupe
    ON audit_logs (dedupe_key, created_at) WHERE dedupe_key IS NOT NULL;

-- --- 3. chain --------------------------------------------------------------
-- NOT unique, and that is a constraint of partitioning rather than a choice: a
-- unique index here would have to include created_at, which would let two rows
-- share a chain_seq in different months and defeat the point. Uniqueness is
-- enforced instead by the audit_chain_heads row lock that allocates the seq —
-- the same argument 015_collab.up.sql makes for collab_documents.head_seq over a
-- SEQUENCE, and for the same reason: a sequence hands 5 to a transaction that
-- commits after 6, and a verifier walking the chain would conclude 5 is missing.
CREATE INDEX idx_audit_logs_chain
    ON audit_logs (workspace_id, chain_seq) WHERE chain_seq IS NOT NULL;

CREATE TABLE audit_chain_heads (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    head_seq     BIGINT NOT NULL DEFAULT 0,
    head_hash    BYTEA,
    -- Last seq shipped off-box by internal/audit's Sink. Everything at or below
    -- it is anchored somewhere the local administrator does not control;
    -- everything above it is protected by nothing but the chain.
    anchored_seq BIGINT NOT NULL DEFAULT 0,
    anchored_at  TIMESTAMPTZ,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- --- 4. query indexes ------------------------------------------------------
CREATE INDEX idx_audit_logs_ws_actor    ON audit_logs (workspace_id, actor_id, created_at DESC);
CREATE INDEX idx_audit_logs_ws_resource ON audit_logs (workspace_id, resource_type, resource_id, created_at DESC);
CREATE INDEX idx_audit_logs_ws_action   ON audit_logs (workspace_id, action, created_at DESC);
