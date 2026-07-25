-- Reverse of 014. Dropping sso_providers cascades to identities, in-flight
-- authorization requests and pending logins, but they are named explicitly so
-- the intent survives a future change to the foreign keys.

ALTER TABLE sessions DROP CONSTRAINT IF EXISTS sessions_auth_method_valid;
ALTER TABLE sessions DROP COLUMN IF EXISTS auth_method;

DROP TABLE IF EXISTS sso_pending_logins;
DROP TABLE IF EXISTS sso_auth_requests;
DROP TABLE IF EXISTS sso_identities;

DROP TRIGGER IF EXISTS sso_providers_updated_at ON sso_providers;
DROP TABLE IF EXISTS sso_providers;
