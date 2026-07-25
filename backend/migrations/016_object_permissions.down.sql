-- Reverses 016. Not lossy: every row in these tables is derived from
-- workspaces/channels/files/channel_members plus acl_grant, and acl_grant is
-- empty until something starts writing explicit grants — after that, dropping
-- it DOES lose the explicit shares, which is why this file names the fact.
--
-- Views first (they depend on the tables and on the functions), then the
-- functions, then the tables children-first.
DROP VIEW IF EXISTS acl_key_expected;
DROP VIEW IF EXISTS acl_object_expected;

DROP FUNCTION IF EXISTS acl_container_key(TEXT);
DROP FUNCTION IF EXISTS acl_parent_segment(TEXT);

DROP TABLE IF EXISTS acl_key;
DROP TABLE IF EXISTS acl_grant;
DROP TABLE IF EXISTS acl_object;
