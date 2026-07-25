-- 020 down. LOSSY: every inbox event and item is destroyed, including the ones
-- produced after the migration ran. The `notifications` rows the up-migration
-- backfilled FROM are untouched and still there, so rolling back restores the
-- old notification list to exactly the state it was in at cutover — anything
-- that arrived in the inbox afterwards is gone, because nothing wrote it to the
-- old table.

DROP TABLE IF EXISTS inbox_digest_state;
DROP TABLE IF EXISTS notification_prefs;
DROP TABLE IF EXISTS inbox_events;
DROP TABLE IF EXISTS inbox_items;
