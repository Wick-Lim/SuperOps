-- Reverse of 050. Lossy in the way that matters and is worth saying: dropping
-- huddle_participants destroys the record of who was in which call and for how
-- long, which nothing else holds. The SFU keeps its own session log, but that
-- is a different system with a different retention policy and no user ids.

DROP TABLE IF EXISTS huddle_webhook_events;
DROP TABLE IF EXISTS huddle_participants;
DROP TABLE IF EXISTS huddles;
