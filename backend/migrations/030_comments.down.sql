-- Reverses 030.
--
-- LOSSY: it drops every comment on every object. There is no way around that —
-- the table IS the comments — and it is stated here rather than discovered.

DROP TRIGGER IF EXISTS trg_comments_shape ON comments;
DROP FUNCTION IF EXISTS comments_enforce_shape();
DROP TRIGGER IF EXISTS trg_comments_updated_at ON comments;
DROP TABLE IF EXISTS comments;
