-- Reverses 064.
DROP INDEX IF EXISTS idx_collab_documents_repair;
ALTER TABLE collab_documents DROP COLUMN IF EXISTS repair_requested_at;
