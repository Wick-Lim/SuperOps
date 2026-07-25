package sso

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Provider is one workspace's identity-provider configuration.
//
// It carries no json tags: the client secret lives in this struct and a
// stray json.Marshal of it would be a credential disclosure. Responses are
// built from ProviderView instead.
type Provider struct {
	ID          string
	WorkspaceID string
	Name        string
	Issuer      string
	ClientID    string

	// clientSecretEnc is the sealed secret exactly as stored. Unexported so
	// that reaching the plaintext has to go through ClientSecret(key), which is
	// the only place the decision "this caller may see it" is made.
	clientSecretEnc []byte

	RedirectURI string
	Scopes      string

	Enabled                 bool
	Enforced                bool
	AllowOwnerPasswordLogin bool
	AllowJIT                bool
	AllowLinking            bool
	RequireVerifiedEmail    bool
	TrustIDPMFA             bool
	RequiredACR             string

	DefaultRole string
	RoleClaim   string
	RoleMapping map[string]string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// HasClientSecret reports whether a secret is stored, without opening it.
func (p *Provider) HasClientSecret() bool { return len(p.clientSecretEnc) > 0 }

// ClientSecret opens the sealed secret for the token exchange.
func (p *Provider) ClientSecret(key []byte) (string, error) {
	return openSecret(key, p.clientSecretEnc)
}

// ProviderView is the API representation. The absence of a secret field is the
// point: there is no code path that can serialize one.
type ProviderView struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	Name        string `json:"name"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"client_id"`
	// ClientSecretSet tells an administrator whether they still need to supply
	// one; the value itself never leaves the database.
	ClientSecretSet bool   `json:"client_secret_set"`
	RedirectURI     string `json:"redirect_uri"`
	Scopes          string `json:"scopes"`

	Enabled                 bool   `json:"enabled"`
	Enforced                bool   `json:"enforced"`
	AllowOwnerPasswordLogin bool   `json:"allow_owner_password_login"`
	AllowJIT                bool   `json:"allow_jit"`
	AllowLinking            bool   `json:"allow_linking"`
	RequireVerifiedEmail    bool   `json:"require_verified_email"`
	TrustIDPMFA             bool   `json:"trust_idp_mfa"`
	RequiredACR             string `json:"required_acr"`

	DefaultRole string            `json:"default_role"`
	RoleClaim   string            `json:"role_claim"`
	RoleMapping map[string]string `json:"role_mapping"`

	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

func (p *Provider) View() ProviderView {
	mapping := p.RoleMapping
	if mapping == nil {
		mapping = map[string]string{}
	}
	return ProviderView{
		ID:                      p.ID,
		WorkspaceID:             p.WorkspaceID,
		Name:                    p.Name,
		Issuer:                  p.Issuer,
		ClientID:                p.ClientID,
		ClientSecretSet:         p.HasClientSecret(),
		RedirectURI:             p.RedirectURI,
		Scopes:                  p.Scopes,
		Enabled:                 p.Enabled,
		Enforced:                p.Enforced,
		AllowOwnerPasswordLogin: p.AllowOwnerPasswordLogin,
		AllowJIT:                p.AllowJIT,
		AllowLinking:            p.AllowLinking,
		RequireVerifiedEmail:    p.RequireVerifiedEmail,
		TrustIDPMFA:             p.TrustIDPMFA,
		RequiredACR:             p.RequiredACR,
		DefaultRole:             p.DefaultRole,
		RoleClaim:               p.RoleClaim,
		RoleMapping:             mapping,
		CreatedAt:               p.CreatedAt,
		UpdatedAt:               p.UpdatedAt,
	}
}

// Identity is a (provider, subject) -> user binding.
type Identity struct {
	ID          string
	ProviderID  string
	UserID      string
	Subject     string
	Email       string
	CreatedAt   time.Time
	LastLoginAt *time.Time
}

// AuthRequest is one in-flight authorization.
type AuthRequest struct {
	StateHash    string
	ProviderID   string
	NonceHash    string
	CodeVerifier string
	ExpiresAt    time.Time
}

// PendingLogin is a verified assertion that must not become a session yet.
type PendingLogin struct {
	ID         string
	ProviderID string
	UserID     string
	Kind       string
	Subject    string
	Email      string
	ExpiresAt  time.Time
}

const (
	// PendingTOTP: the identity is bound, the account has a second factor.
	PendingTOTP = "totp"
	// PendingLink: the assertion matches an existing local account that is not
	// bound to this provider, and binding needs that account's password.
	PendingLink = "link"
)

// LocalAccount is the minimum the linking decision needs about an existing
// user. It deliberately does not carry the password hash: the comparison
// happens in internal/auth, which owns credentials.
type LocalAccount struct {
	ID          string
	Email       string
	IsActive    bool
	HasPassword bool
}

const providerColumns = `id, workspace_id, name, issuer, client_id, client_secret_enc, redirect_uri, scopes,
	enabled, enforced, allow_owner_password_login, allow_jit, allow_linking, require_verified_email,
	trust_idp_mfa, required_acr, default_role, role_claim, role_mapping, created_at, updated_at`

type Repository struct {
	pool *pgxpool.Pool
}

func NewRepository(pool *pgxpool.Pool) *Repository {
	return &Repository{pool: pool}
}

func isUniqueViolation(err error) bool {
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "23505"
}

func scanProvider(row pgx.Row) (*Provider, error) {
	var p Provider
	var mapping []byte
	err := row.Scan(&p.ID, &p.WorkspaceID, &p.Name, &p.Issuer, &p.ClientID, &p.clientSecretEnc,
		&p.RedirectURI, &p.Scopes, &p.Enabled, &p.Enforced, &p.AllowOwnerPasswordLogin,
		&p.AllowJIT, &p.AllowLinking, &p.RequireVerifiedEmail, &p.TrustIDPMFA, &p.RequiredACR,
		&p.DefaultRole, &p.RoleClaim, &mapping, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan sso provider: %w", err)
	}
	if len(mapping) > 0 {
		if err := json.Unmarshal(mapping, &p.RoleMapping); err != nil {
			return nil, fmt.Errorf("decode role mapping: %w", err)
		}
	}
	return &p, nil
}

func (r *Repository) GetProviderByWorkspace(ctx context.Context, workspaceID string) (*Provider, error) {
	return scanProvider(r.pool.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM sso_providers WHERE workspace_id = $1`, workspaceID))
}

func (r *Repository) GetProviderByID(ctx context.Context, id string) (*Provider, error) {
	return scanProvider(r.pool.QueryRow(ctx,
		`SELECT `+providerColumns+` FROM sso_providers WHERE id = $1`, id))
}

// GetProviderByWorkspaceSlug resolves the provider a sign-in page needs from
// the only identifier an unauthenticated visitor has: the workspace slug.
// A subquery rather than a join so the column list stays unqualified — there is
// exactly one provider row per workspace.
func (r *Repository) GetProviderByWorkspaceSlug(ctx context.Context, slug string) (*Provider, error) {
	return scanProvider(r.pool.QueryRow(ctx,
		`SELECT `+providerColumns+`
		   FROM sso_providers
		  WHERE workspace_id = (SELECT id FROM workspaces WHERE slug = $1)`, slug))
}

// SaveProviderInput is one PUT of the configuration. Every field is written;
// ClientSecret is the exception, because "leave the stored secret alone" has to
// be expressible without the caller knowing it.
type SaveProviderInput struct {
	WorkspaceID string
	Name        string
	Issuer      string
	ClientID    string
	// ClientSecretEnc nil means "keep whatever is stored"; a zero-length
	// non-nil slice means "clear it" (the provider is a public client).
	ClientSecretEnc []byte
	RedirectURI     string
	Scopes          string

	Enabled                 bool
	AllowOwnerPasswordLogin bool
	AllowJIT                bool
	AllowLinking            bool
	RequireVerifiedEmail    bool
	TrustIDPMFA             bool
	RequiredACR             string

	DefaultRole string
	RoleClaim   string
	RoleMapping map[string]string
}

// SaveProvider inserts or replaces a workspace's configuration.
//
// Enforcement is deliberately not settable here: it has a lockout guard of its
// own (SetEnforced) that a general "update the config" call must not be able to
// route around. Disabling the provider does clear it, because the CHECK
// constraint forbids enforced-but-disabled and, more importantly, disabling is
// the way out of a broken enforcement.
func (r *Repository) SaveProvider(ctx context.Context, in SaveProviderInput) (*Provider, error) {
	mapping, err := json.Marshal(in.RoleMapping)
	if err != nil || in.RoleMapping == nil {
		mapping = []byte("{}")
	}

	// COALESCE keeps the stored secret when the caller sent none. The cast is
	// explicit because a nil []byte parameter is otherwise untyped.
	row := r.pool.QueryRow(ctx,
		`INSERT INTO sso_providers
		   (id, workspace_id, name, issuer, client_id, client_secret_enc, redirect_uri, scopes,
		    enabled, allow_owner_password_login, allow_jit, allow_linking, require_verified_email,
		    trust_idp_mfa, required_acr, default_role, role_claim, role_mapping)
		 VALUES ($1, $2, $3, $4, $5, $6::bytea, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $18::jsonb)
		 ON CONFLICT (workspace_id) DO UPDATE SET
		    name = EXCLUDED.name,
		    issuer = EXCLUDED.issuer,
		    client_id = EXCLUDED.client_id,
		    client_secret_enc = COALESCE(EXCLUDED.client_secret_enc, sso_providers.client_secret_enc),
		    redirect_uri = EXCLUDED.redirect_uri,
		    scopes = EXCLUDED.scopes,
		    enabled = EXCLUDED.enabled,
		    enforced = sso_providers.enforced AND EXCLUDED.enabled,
		    allow_owner_password_login = EXCLUDED.allow_owner_password_login,
		    allow_jit = EXCLUDED.allow_jit,
		    allow_linking = EXCLUDED.allow_linking,
		    require_verified_email = EXCLUDED.require_verified_email,
		    trust_idp_mfa = EXCLUDED.trust_idp_mfa,
		    required_acr = EXCLUDED.required_acr,
		    default_role = EXCLUDED.default_role,
		    role_claim = EXCLUDED.role_claim,
		    role_mapping = EXCLUDED.role_mapping
		 RETURNING `+providerColumns,
		uuid.NewString(), in.WorkspaceID, in.Name, in.Issuer, in.ClientID, nilIfEmptyBytes(in.ClientSecretEnc),
		in.RedirectURI, in.Scopes, in.Enabled, in.AllowOwnerPasswordLogin, in.AllowJIT, in.AllowLinking,
		in.RequireVerifiedEmail, in.TrustIDPMFA, in.RequiredACR, in.DefaultRole, in.RoleClaim, string(mapping),
	)
	p, err := scanProvider(row)
	if err != nil {
		return nil, fmt.Errorf("save sso provider: %w", err)
	}
	return p, nil
}

// nilIfEmptyBytes distinguishes "keep the stored secret" (nil) from "store an
// empty secret", which is not a thing: a public client stores NULL either way.
func nilIfEmptyBytes(b []byte) interface{} {
	if len(b) == 0 {
		return nil
	}
	return b
}

// ClearClientSecret drops the stored secret, for a provider reconfigured as a
// public (PKCE-only) client.
func (r *Repository) ClearClientSecret(ctx context.Context, providerID string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE sso_providers SET client_secret_enc = NULL WHERE id = $1`, providerID); err != nil {
		return fmt.Errorf("clear client secret: %w", err)
	}
	return nil
}

func (r *Repository) DeleteProvider(ctx context.Context, workspaceID string) (bool, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sso_providers WHERE workspace_id = $1`, workspaceID)
	if err != nil {
		return false, fmt.Errorf("delete sso provider: %w", err)
	}
	return tag.RowsAffected() > 0, nil
}

// SetEnforced flips password-login enforcement. The guard that stops an
// administrator locking themselves out lives in the service, not here.
func (r *Repository) SetEnforced(ctx context.Context, providerID string, enforced bool) error {
	tag, err := r.pool.Exec(ctx,
		`UPDATE sso_providers SET enforced = $2 WHERE id = $1 AND (enabled OR NOT $2)`, providerID, enforced)
	if err != nil {
		return fmt.Errorf("set sso enforcement: %w", err)
	}
	if tag.RowsAffected() == 0 {
		return ErrProviderDisabled
	}
	return nil
}

// ---------------------------------------------------------------------------
// In-flight authorization requests
// ---------------------------------------------------------------------------

func (r *Repository) CreateAuthRequest(ctx context.Context, req *AuthRequest) error {
	if _, err := r.pool.Exec(ctx,
		`INSERT INTO sso_auth_requests (state_hash, provider_id, nonce_hash, code_verifier, expires_at)
		 VALUES ($1, $2, $3, $4, $5)`,
		req.StateHash, req.ProviderID, req.NonceHash, req.CodeVerifier, req.ExpiresAt,
	); err != nil {
		return fmt.Errorf("create sso auth request: %w", err)
	}
	return nil
}

// ConsumeAuthRequest atomically spends a state token.
//
// DELETE ... RETURNING rather than SELECT-then-DELETE: two callbacks racing on
// the same state must not both proceed, and an expired row must not be
// consumable at all. Returns (nil, nil) when the state is unknown, expired or
// already spent — all of which the caller answers identically.
func (r *Repository) ConsumeAuthRequest(ctx context.Context, stateHash string) (*AuthRequest, error) {
	var req AuthRequest
	err := r.pool.QueryRow(ctx,
		`DELETE FROM sso_auth_requests
		  WHERE state_hash = $1 AND expires_at > NOW()
		 RETURNING state_hash, provider_id, nonce_hash, code_verifier, expires_at`,
		stateHash,
	).Scan(&req.StateHash, &req.ProviderID, &req.NonceHash, &req.CodeVerifier, &req.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume sso auth request: %w", err)
	}
	return &req, nil
}

// CleanExpiredAuthRequests drops abandoned sign-ins. The background worker runs
// this on a timer; nothing else removes a row that was started and never
// finished.
func (r *Repository) CleanExpiredAuthRequests(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sso_auth_requests WHERE expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("clean expired sso auth requests: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Half-finished logins
// ---------------------------------------------------------------------------

func (r *Repository) CreatePendingLogin(ctx context.Context, p *PendingLogin, tokenHash string) error {
	if p.ID == "" {
		p.ID = uuid.NewString()
	}
	if _, err := r.pool.Exec(ctx,
		`INSERT INTO sso_pending_logins (id, token_hash, provider_id, user_id, kind, subject, email, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
		p.ID, tokenHash, p.ProviderID, p.UserID, p.Kind, p.Subject, p.Email, p.ExpiresAt,
	); err != nil {
		return fmt.Errorf("create sso pending login: %w", err)
	}
	return nil
}

// ConsumePendingLogin spends a second-step token, atomically and once.
func (r *Repository) ConsumePendingLogin(ctx context.Context, tokenHash, kind string) (*PendingLogin, error) {
	var p PendingLogin
	err := r.pool.QueryRow(ctx,
		`DELETE FROM sso_pending_logins
		  WHERE token_hash = $1 AND kind = $2 AND expires_at > NOW()
		 RETURNING id, provider_id, user_id, kind, subject, email, expires_at`,
		tokenHash, kind,
	).Scan(&p.ID, &p.ProviderID, &p.UserID, &p.Kind, &p.Subject, &p.Email, &p.ExpiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("consume sso pending login: %w", err)
	}
	return &p, nil
}

func (r *Repository) CleanExpiredPendingLogins(ctx context.Context) (int64, error) {
	tag, err := r.pool.Exec(ctx, `DELETE FROM sso_pending_logins WHERE expires_at < NOW()`)
	if err != nil {
		return 0, fmt.Errorf("clean expired sso pending logins: %w", err)
	}
	return tag.RowsAffected(), nil
}

// ---------------------------------------------------------------------------
// Identities
// ---------------------------------------------------------------------------

const identityColumns = `id, provider_id, user_id, subject, email, created_at, last_login_at`

func scanIdentity(row pgx.Row) (*Identity, error) {
	var i Identity
	err := row.Scan(&i.ID, &i.ProviderID, &i.UserID, &i.Subject, &i.Email, &i.CreatedAt, &i.LastLoginAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("scan sso identity: %w", err)
	}
	return &i, nil
}

func (r *Repository) GetIdentity(ctx context.Context, providerID, subject string) (*Identity, error) {
	return scanIdentity(r.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM sso_identities WHERE provider_id = $1 AND subject = $2`,
		providerID, subject))
}

func (r *Repository) GetIdentityByUser(ctx context.Context, providerID, userID string) (*Identity, error) {
	return scanIdentity(r.pool.QueryRow(ctx,
		`SELECT `+identityColumns+` FROM sso_identities WHERE provider_id = $1 AND user_id = $2`,
		providerID, userID))
}

// LinkIdentity binds a subject to an existing local account.
//
// The unique constraints do the work: a subject already bound to somebody else,
// or an account already bound to a different subject, both surface as
// ErrIdentityTaken rather than as a silent second row.
func (r *Repository) LinkIdentity(ctx context.Context, providerID, userID, subject, email string) error {
	_, err := r.pool.Exec(ctx,
		`INSERT INTO sso_identities (id, provider_id, user_id, subject, email, last_login_at)
		 VALUES ($1, $2, $3, $4, $5, NOW())`,
		uuid.NewString(), providerID, userID, subject, email)
	if isUniqueViolation(err) {
		return ErrIdentityTaken
	}
	if err != nil {
		return fmt.Errorf("link sso identity: %w", err)
	}
	return nil
}

// TouchIdentity records the login and refreshes the administrator-facing email.
func (r *Repository) TouchIdentity(ctx context.Context, identityID, email string) error {
	if _, err := r.pool.Exec(ctx,
		`UPDATE sso_identities SET last_login_at = NOW(), email = $2 WHERE id = $1`,
		identityID, email); err != nil {
		return fmt.Errorf("touch sso identity: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Accounts
// ---------------------------------------------------------------------------

// FindLocalAccountByEmail looks an account up case-insensitively.
//
// Case-insensitively on purpose: users.email is unique on the exact string, so
// "Victim@corp.com" and "victim@corp.com" can both exist. An exact-match lookup
// here would miss the second one and JIT-provision a duplicate account for an
// address that is already taken — which is the takeover this whole path exists
// to prevent, arrived at from the other direction.
//
// More than one match is refused rather than resolved: there is no correct
// answer to "which of these two accounts did the provider mean".
func (r *Repository) FindLocalAccountByEmail(ctx context.Context, email string) (*LocalAccount, error) {
	rows, err := r.pool.Query(ctx,
		`SELECT id, email, is_active, COALESCE(password_hash, '') <> ''
		   FROM users WHERE lower(email) = lower($1) ORDER BY created_at LIMIT 2`, email)
	if err != nil {
		return nil, fmt.Errorf("find local account: %w", err)
	}
	defer rows.Close()

	var found []LocalAccount
	for rows.Next() {
		var a LocalAccount
		if err := rows.Scan(&a.ID, &a.Email, &a.IsActive, &a.HasPassword); err != nil {
			return nil, fmt.Errorf("scan local account: %w", err)
		}
		found = append(found, a)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find local account: %w", err)
	}

	switch len(found) {
	case 0:
		return nil, nil
	case 1:
		return &found[0], nil
	default:
		return nil, ErrEmailAmbiguous
	}
}

// IsActive re-reads the deactivation flag. Everything that mints a session
// calls it: the flag may have flipped between the assertion and now, and an SSO
// login that skipped it would be a way back in for a disabled account.
func (r *Repository) IsActive(ctx context.Context, userID string) (bool, error) {
	var active bool
	err := r.pool.QueryRow(ctx, `SELECT is_active FROM users WHERE id = $1`, userID).Scan(&active)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("load user state: %w", err)
	}
	return active, nil
}

// ProvisionUser creates an account, its workspace membership and its identity
// binding in one transaction.
//
// One transaction because every partial outcome is worse than a failure: a user
// with no membership cannot see anything and cannot be found by an admin, a
// membership with no identity would be re-created on every login, and an
// identity with no user is a foreign key violation waiting to happen.
func (r *Repository) ProvisionUser(ctx context.Context, in ProvisionInput) (string, error) {
	userID := uuid.NewString()

	err := database.WithTx(ctx, r.pool, func(tx pgx.Tx) error {
		username, err := uniqueUsername(ctx, tx, in.Email, in.PreferredUsername)
		if err != nil {
			return err
		}

		// password_hash stays NULL: this account has no password, and
		// auth.Login already refuses an account without one. That is what makes
		// a JIT account unable to be password-guessed.
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
			 VALUES ($1, $2, $3, $4, NULL, TRUE)`,
			userID, in.Email, username, in.FullName,
		); err != nil {
			if isUniqueViolation(err) {
				// The address was taken between the lookup and here. Refusing
				// is correct: the alternative is linking without proof.
				return ErrEmailAmbiguous
			}
			return fmt.Errorf("create sso user: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
			 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
			in.WorkspaceID, userID, in.Role,
		); err != nil {
			return fmt.Errorf("add workspace member: %w", err)
		}

		// Same courtesy the invite flow extends: land in #general if there is
		// one, so a provisioned account is not an empty screen.
		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role)
			 SELECT id, $2, 'member' FROM channels WHERE workspace_id = $1 AND slug = 'general' LIMIT 1
			 ON CONFLICT DO NOTHING`,
			in.WorkspaceID, userID,
		); err != nil {
			return fmt.Errorf("join general channel: %w", err)
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO sso_identities (id, provider_id, user_id, subject, email, last_login_at)
			 VALUES ($1, $2, $3, $4, $5, NOW())`,
			uuid.NewString(), in.ProviderID, userID, in.Subject, in.Email,
		); err != nil {
			if isUniqueViolation(err) {
				return ErrIdentityTaken
			}
			return fmt.Errorf("create sso identity: %w", err)
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return userID, nil
}

// ProvisionInput is a just-in-time account creation.
type ProvisionInput struct {
	ProviderID        string
	WorkspaceID       string
	Subject           string
	Email             string
	FullName          string
	PreferredUsername string
	Role              string
}

// EnsureMembership re-asserts what the provider says about this user.
//
// Two things it does NOT do: create an owner (ownership moves only through
// transfer-ownership), and demote one (an IdP group that happens not to map to
// "owner" must not be able to strip the workspace's owner of their role).
func (r *Repository) EnsureMembership(ctx context.Context, workspaceID, userID, role string, syncRole bool) (added bool, changedTo string, err error) {
	tag, err := r.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
		 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
		workspaceID, userID, role)
	if err != nil {
		return false, "", fmt.Errorf("ensure workspace member: %w", err)
	}
	if tag.RowsAffected() > 0 {
		return true, role, nil
	}
	if !syncRole {
		return false, "", nil
	}

	var updated string
	err = r.pool.QueryRow(ctx,
		`UPDATE workspace_members SET role = $3
		  WHERE workspace_id = $1 AND user_id = $2 AND role <> $3 AND role <> 'owner'
		 RETURNING role`,
		workspaceID, userID, role).Scan(&updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, "", nil
	}
	if err != nil {
		return false, "", fmt.Errorf("sync workspace role: %w", err)
	}
	return false, updated, nil
}

// uniqueUsername derives a username from the provider's assertion and makes it
// unique. users.username is globally unique, so a colliding directory (two
// people called "alex" in two workspaces) must not fail the login.
func uniqueUsername(ctx context.Context, tx pgx.Tx, email, preferred string) (string, error) {
	base := sanitizeUsername(preferred)
	if base == "" {
		base = sanitizeUsername(localPart(email))
	}
	if base == "" {
		base = "user"
	}

	candidate := base
	for attempt := 0; attempt < 12; attempt++ {
		var taken bool
		if err := tx.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM users WHERE lower(username) = lower($1))`, candidate).Scan(&taken); err != nil {
			return "", fmt.Errorf("check username: %w", err)
		}
		if !taken {
			return candidate, nil
		}
		suffix, err := randomSuffix()
		if err != nil {
			return "", err
		}
		candidate = base + "-" + suffix
	}
	return "", errors.New("could not derive a free username")
}
