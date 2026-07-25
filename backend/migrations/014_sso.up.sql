-- 014: OIDC single sign-on.
--
-- Four tables and one column, in the order they matter:
--
--   sso_providers      per-workspace IdP configuration (one deployment serves
--                      several organizations, so this cannot be global config)
--   sso_identities     the (provider, subject) -> user binding. THE binding is
--                      the subject, never the email address.
--   sso_auth_requests  one row per in-flight authorization: state, nonce and
--                      the PKCE verifier, consumed exactly once
--   sso_pending_logins a callback that verified but must not yet mint a
--                      session: the account has 2FA, or the address collides
--                      with an existing local account and needs proof
--   sessions.auth_method  how the session was obtained, so enforcement can cut
--                      password sessions without cutting SSO ones
--
-- Why the identity is keyed on subject and not on email:
-- an OIDC `sub` is stable and issuer-scoped; an email address is neither. If
-- the binding were the address, then any provider that lets a user set an
-- unverified address — or any admin who can create a user in their own
-- directory — could take over an existing SuperOps account by claiming its
-- address. The email is stored on the identity row only as a human-readable
-- reference for administrators.

-- ---------------------------------------------------------------------------
-- Providers
-- ---------------------------------------------------------------------------

CREATE TABLE sso_providers (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- Display name for the sign-in button ("Acme Okta").
    name   TEXT NOT NULL DEFAULT '',
    -- The issuer identifier, exactly as it appears in the `iss` claim.
    -- Discovery is issuer + "/.well-known/openid-configuration"; the document's
    -- own `issuer` field must equal this value or the provider is rejected.
    issuer TEXT NOT NULL,

    client_id TEXT NOT NULL,
    -- AES-256-GCM ciphertext (nonce || sealed), keyed by SSO_SECRET_KEY. NULL
    -- for a public client, which is legitimate with PKCE. The plaintext is
    -- never selected by any read path that feeds an API response.
    client_secret_enc BYTEA,

    -- Where the IdP sends the browser back. Registered at the IdP, so it is
    -- configuration rather than something the client may choose per request —
    -- a request-supplied redirect_uri is an open redirector.
    redirect_uri TEXT NOT NULL,
    scopes       TEXT NOT NULL DEFAULT 'openid email profile',

    enabled  BOOLEAN NOT NULL DEFAULT FALSE,
    -- enforced disables password login for every member of this workspace.
    -- See allow_owner_password_login for the lockout escape hatch.
    enforced BOOLEAN NOT NULL DEFAULT FALSE,

    -- The break-glass. With enforcement on and the IdP misconfigured or down,
    -- nobody can sign in — including the one person who could turn enforcement
    -- back off. Workspace owners therefore keep password login by default.
    -- Setting this FALSE is a deliberate choice to have no way back in except
    -- direct database access.
    allow_owner_password_login BOOLEAN NOT NULL DEFAULT TRUE,

    -- JIT provisioning: create the account and the workspace membership on
    -- first login.
    allow_jit BOOLEAN NOT NULL DEFAULT TRUE,
    -- Whether a first-time SSO identity may be bound to an existing local
    -- account at all. Binding ALWAYS requires the local password (and second
    -- factor); this switch turns even that off for deployments that want SSO
    -- accounts and local accounts kept strictly disjoint.
    allow_linking BOOLEAN NOT NULL DEFAULT TRUE,

    -- Require the `email_verified` claim to be true. Default TRUE, and it
    -- should stay TRUE for any provider whose users can set their own address.
    -- Entra ID does not emit the claim at all, which is the only reason this is
    -- an option; turning it off asserts "this directory is authoritative for
    -- every address it emits".
    require_verified_email BOOLEAN NOT NULL DEFAULT TRUE,

    -- Accept the IdP's own multi-factor step in place of the local TOTP prompt,
    -- but only when the ID token actually carries evidence of one (amr contains
    -- an RFC 8176 multi-factor value, or acr equals required_acr). Without
    -- evidence the local second factor is still required.
    trust_idp_mfa BOOLEAN NOT NULL DEFAULT FALSE,
    required_acr  TEXT NOT NULL DEFAULT '',

    -- Role assigned on JIT provisioning when the provider asserts nothing.
    -- 'owner' is deliberately absent: ownership moves only through
    -- POST /workspaces/{id}/transfer-ownership.
    default_role TEXT NOT NULL DEFAULT 'member',
    -- Optional claim carrying group/role values ("groups", "roles"), and the
    -- mapping from those values onto workspace roles: {"acme-admins":"admin"}.
    role_claim   TEXT NOT NULL DEFAULT '',
    role_mapping JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- One provider per workspace. Several would need a selector in the start
    -- request and a rule for which one enforcement refers to; neither exists.
    CONSTRAINT sso_providers_workspace_key UNIQUE (workspace_id),
    CONSTRAINT sso_providers_default_role_valid CHECK (default_role IN ('admin', 'member', 'guest')),
    CONSTRAINT sso_providers_issuer_len CHECK (char_length(issuer) BETWEEN 1 AND 512),
    CONSTRAINT sso_providers_client_id_len CHECK (char_length(client_id) BETWEEN 1 AND 512),
    CONSTRAINT sso_providers_redirect_len CHECK (char_length(redirect_uri) BETWEEN 1 AND 1024),
    -- Enforcement without a working provider is a lockout with no upside.
    CONSTRAINT sso_providers_enforced_requires_enabled CHECK (NOT enforced OR enabled)
);

COMMENT ON COLUMN sso_providers.client_secret_enc IS
    'AES-256-GCM sealed OIDC client secret (nonce||ciphertext), key from SSO_SECRET_KEY. Never returned by the API.';

CREATE TRIGGER sso_providers_updated_at
    BEFORE UPDATE ON sso_providers
    FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Identities
-- ---------------------------------------------------------------------------

CREATE TABLE sso_identities (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    provider_id UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- The `sub` claim. Issuer-scoped and stable; the provider row supplies the
    -- issuer half of the pair.
    subject TEXT NOT NULL,
    -- Last address the provider asserted, for administrators reading the table.
    -- Nothing authenticates against it.
    email   TEXT NOT NULL DEFAULT '',

    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_login_at TIMESTAMPTZ,

    -- One user per subject: without this a second row could point the same
    -- directory account at a second local user.
    CONSTRAINT sso_identities_provider_subject_key UNIQUE (provider_id, subject),
    -- One identity per user per provider.
    CONSTRAINT sso_identities_provider_user_key UNIQUE (provider_id, user_id),
    CONSTRAINT sso_identities_subject_len CHECK (char_length(subject) BETWEEN 1 AND 255)
);

CREATE INDEX idx_sso_identities_user ON sso_identities (user_id);

-- ---------------------------------------------------------------------------
-- In-flight authorization requests
-- ---------------------------------------------------------------------------

CREATE TABLE sso_auth_requests (
    -- SHA-256 of the state token; the plaintext lives only in the browser and
    -- in the IdP redirect. Same treatment as every other bearer token here.
    state_hash  TEXT PRIMARY KEY,
    provider_id UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,

    -- SHA-256 of the nonce. The plaintext went out in the authorization URL and
    -- comes back inside the ID token, so a hash is enough to compare.
    nonce_hash TEXT NOT NULL,
    -- The PKCE code_verifier must be replayed verbatim to the token endpoint,
    -- so unlike the others it cannot be hashed. It is single-use and expires in
    -- minutes, and it is worthless without the matching authorization code.
    code_verifier TEXT NOT NULL,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT sso_auth_requests_verifier_len CHECK (char_length(code_verifier) BETWEEN 43 AND 128)
);

-- Consumption is DELETE ... RETURNING on the primary key, so the only index
-- needed beyond the PK is the sweep of abandoned rows.
CREATE INDEX idx_sso_auth_requests_expires ON sso_auth_requests (expires_at);

-- ---------------------------------------------------------------------------
-- Half-finished logins
-- ---------------------------------------------------------------------------

CREATE TABLE sso_pending_logins (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    token_hash  TEXT NOT NULL UNIQUE,
    provider_id UUID NOT NULL REFERENCES sso_providers(id) ON DELETE CASCADE,
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,

    -- 'totp': the identity is bound and the assertion verified, but the account
    --         has 2FA and SSO must not be a way around it.
    -- 'link' : the assertion verified and its address matches an existing local
    --          account that is NOT bound to this provider. Completing requires
    --          that account's password, so an IdP that can assert an arbitrary
    --          address still cannot take the account over.
    kind    TEXT NOT NULL,
    subject TEXT NOT NULL,
    email   TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL,

    CONSTRAINT sso_pending_logins_kind_valid CHECK (kind IN ('totp', 'link'))
);

CREATE INDEX idx_sso_pending_logins_expires ON sso_pending_logins (expires_at);
CREATE INDEX idx_sso_pending_logins_user ON sso_pending_logins (user_id);
CREATE INDEX idx_sso_pending_logins_provider ON sso_pending_logins (provider_id);

-- ---------------------------------------------------------------------------
-- How a session was obtained
-- ---------------------------------------------------------------------------
--
-- Turning enforcement on has to invalidate password sessions without touching
-- SSO ones, and refresh-token rotation has to re-evaluate the policy for a
-- session that predates it. Neither is expressible unless the session records
-- which factor produced it. Existing rows are password sessions by definition:
-- there was no other way to get one.

ALTER TABLE sessions ADD COLUMN auth_method TEXT NOT NULL DEFAULT 'password';
ALTER TABLE sessions ADD CONSTRAINT sessions_auth_method_valid
    CHECK (auth_method IN ('password', 'sso'));
