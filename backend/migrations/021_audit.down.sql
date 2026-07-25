-- 021 down. LOSSY, in three separate ways, and all three are irreversible:
--
--   * resource_id goes back to UUID. Every value that is not a uuid literal —
--     which is the entire reason the column was widened — becomes NULL. The
--     original value is not recoverable from anywhere else.
--   * The hash chain (chain_seq, prev_hash, hash) and every anchor recorded in
--     audit_chain_heads are dropped. Re-running the up migration afterwards
--     starts every workspace's chain again at zero, so nothing written before
--     the rollback is covered by tamper-evidence any more.
--   * Coalesced rows collapse to one row with their count discarded, because
--     the 005 schema has nowhere to put event_count or last_at. Fifty downloads
--     recorded as one row with count 50 come back as one row, full stop.
--
-- Rows themselves survive: the partitions are copied back into a single
-- unpartitioned table before they are dropped.

CREATE TABLE audit_logs_unpartitioned (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID REFERENCES workspaces(id) ON DELETE SET NULL,
    actor_id      UUID REFERENCES users(id) ON DELETE SET NULL,
    action        TEXT NOT NULL,
    resource_type TEXT NOT NULL,
    resource_id   UUID,
    metadata      JSONB NOT NULL DEFAULT '{}',
    ip_address    INET,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

INSERT INTO audit_logs_unpartitioned
    (id, workspace_id, actor_id, action, resource_type, resource_id, metadata, ip_address, created_at)
SELECT DISTINCT ON (id)
       id, workspace_id, actor_id, action, resource_type,
       CASE WHEN resource_id ~ '^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$'
            THEN resource_id::uuid END,
       metadata, ip_address, created_at
  FROM audit_logs
 ORDER BY id, created_at;

DROP TABLE audit_logs;
DROP TABLE IF EXISTS audit_chain_heads;

ALTER TABLE audit_logs_unpartitioned RENAME TO audit_logs;

-- Constraint names are derived from the table name at CREATE time, so restore
-- them explicitly. Without this the up migration cannot be replayed: it drops
-- `audit_logs_pkey` by name, and a rolled-back-then-reapplied database would be
-- carrying `audit_logs_unpartitioned_pkey` instead.
ALTER TABLE audit_logs RENAME CONSTRAINT audit_logs_unpartitioned_pkey TO audit_logs_pkey;
ALTER TABLE audit_logs RENAME CONSTRAINT audit_logs_unpartitioned_workspace_id_fkey TO audit_logs_workspace_id_fkey;
ALTER TABLE audit_logs RENAME CONSTRAINT audit_logs_unpartitioned_actor_id_fkey TO audit_logs_actor_id_fkey;

-- 005's workspace index. idx_audit_logs_actor is deliberately not restored:
-- 009_hardening dropped it as unusable and this migration did not bring it back
-- in that shape.
CREATE INDEX idx_audit_logs_workspace ON audit_logs (workspace_id, created_at DESC);
