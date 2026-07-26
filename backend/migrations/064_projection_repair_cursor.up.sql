-- 064: the projection repair sweep needs a memory.
--
-- FindStaleProjections orders by updated_at and takes the first N, and the
-- query has no state — so the batch is a pure function of the table and the
-- OLDEST entries never leave it. A document whose room is permanently empty can
-- never be repaired (only a client holding the CRDT can produce a projection),
-- so it sits at the front of that queue forever and everything behind it
-- starves. With the job's batch of 100, a tenant that accumulates 100
-- abandoned documents stops repairing anything else, silently, and the WARN
-- line keeps reporting the same number every ten minutes as though nothing were
-- happening — which is exactly what would be happening.
--
-- Least-recently-asked-first is the whole fix. A document that was just asked
-- goes to the back; one that has never been asked goes to the front. An
-- unanswerable document still gets its turn, it just stops taking everybody
-- else's.
ALTER TABLE collab_documents
    ADD COLUMN repair_requested_at TIMESTAMPTZ;

-- The sweep's ordering key, WITH THE NULLS ORDERING THE QUERY ASKS FOR.
--
-- I removed this index once, having measured it going unused and concluded that
-- no index could help. The measurement was right and the conclusion was wrong:
-- Postgres defaults ASC to NULLS LAST, the query asks for NULLS FIRST, and a
-- btree cannot serve an ordering it was not built in. Spelling the ordering out
-- is the whole difference.
--
-- Measured here on 400,000 rows, the shipped statement verbatim:
--   without         Parallel Seq Scan, 400,000 rows, top-N heapsort
--   with, matching  Index Scan, 100 rows read, 0.15 ms
--
-- The write cost is real but not new: idx_collab_documents_updated already
-- covers the same column and every CRDT append already bumps it, so this is one
-- more entry per append rather than a new class of work — and the alternative
-- is a full-table scan and an external sort every ten minutes, growing with the
-- table.
CREATE INDEX idx_collab_documents_repair
    ON collab_documents (repair_requested_at ASC NULLS FIRST, updated_at);
