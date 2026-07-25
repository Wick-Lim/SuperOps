-- Reverses 012_device_tokens.up.sql.
--
-- Dropping the table discards every registered device. Rolling forward again
-- recovers on its own: the client re-registers its token on the next launch.

DROP TABLE IF EXISTS device_tokens;
