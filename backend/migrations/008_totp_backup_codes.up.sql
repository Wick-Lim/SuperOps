-- #32: 2FA backup recovery codes (totp_secret/totp_enabled added in 007)
CREATE TABLE totp_backup_codes (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash  TEXT NOT NULL,
    used       BOOLEAN NOT NULL DEFAULT FALSE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_totp_backup_user ON totp_backup_codes (user_id) WHERE used = FALSE;
