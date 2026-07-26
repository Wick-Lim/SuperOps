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

-- The sweep's ordering key. NULLS FIRST is the default for ASC, which is what
-- we want — never asked outranks asked-a-year-ago.
CREATE INDEX idx_collab_documents_repair
    ON collab_documents (repair_requested_at, updated_at);
