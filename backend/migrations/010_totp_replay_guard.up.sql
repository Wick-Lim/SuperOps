-- 010: TOTP replay protection.
--
-- ValidateTOTP accepts time steps -1/0/+1, so a single 6-digit code stayed
-- usable for a full 90 seconds with no record that it had already been spent.
-- A shoulder-surfed or phished code could therefore be replayed.
--
-- totp_last_step holds the highest time step this user has ever successfully
-- authenticated with (unix_seconds / 30). Verification is a guarded UPDATE
-- (... WHERE totp_last_step < $step) so acceptance is both single-use and
-- monotonic, and concurrent logins racing on the same code cannot both win.
-- 0 means "never used a code", which is below every real step.

ALTER TABLE users ADD COLUMN totp_last_step BIGINT NOT NULL DEFAULT 0;
