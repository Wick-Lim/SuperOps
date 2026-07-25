-- Reverse of 035. The projection is derived state, so dropping these tables
-- costs search and mobile rendering until the next projection and costs zero
-- content — which is the property the up migration's header claims and this is
-- where it is cashed.
--
-- comment_anchors goes with them: an anchor without its projection is a
-- position in a document nothing can render, and the comments themselves are
-- migration 030's and survive untouched.

DROP INDEX IF EXISTS idx_collab_documents_updated;

DROP TABLE IF EXISTS comment_anchors;
DROP TABLE IF EXISTS file_projection_refs;
DROP TABLE IF EXISTS file_projections;
