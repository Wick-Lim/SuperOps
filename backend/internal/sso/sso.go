// Package sso implements OpenID Connect Authorization Code flow with PKCE
// against a generic provider — Okta, Entra ID, Google Workspace, Keycloak and
// Authentik all speak it, so there is no per-vendor code path here.
//
// Standard library only: discovery, JWKS, ID token signature verification and
// the token exchange are all written against net/http, crypto/* and
// encoding/json. The one JWT implementation already vendored
// (golang-jwt/jwt/v5) is deliberately not used — it is wired for the HS256
// access tokens this service mints for itself, and an ID token is the opposite
// situation: an asymmetric signature from a key set we fetch and rotate.
//
// # What authenticates
//
// The binding stored in sso_identities is (provider, `sub`). Never the email
// address. An OIDC subject is issuer-scoped and stable; an address is neither,
// and treating one as an identifier is the classic account-takeover route into
// a system like this one.
//
// # Account linking
//
// The dangerous case is a first-time SSO login whose asserted address already
// belongs to a local password account. Three answers were possible:
//
//   - Link silently. This is what most implementations do and it is an
//     account-takeover primitive: anyone who can make the IdP assert
//     victim@company.com — a self-service directory, a second tenant, an
//     unverified alias, a compromised IdP admin — inherits the SuperOps
//     account, its workspaces and its history. The `email_verified` claim
//     narrows this but does not close it: verified means the *provider*
//     believes the address, not that the provider is authoritative for the
//     domain.
//   - Link on a "yes, this is me" click. Worthless. The click happens inside
//     the session of whoever just authenticated at the IdP, i.e. the attacker.
//     A confirmation that the attacker performs confirms nothing.
//   - Link on proof of the local credential. What this package does: the
//     callback stops with a `link` challenge and the local account's password
//     (plus its second factor, if enrolled) is required to bind the identity.
//
// So: `email_verified` is required (with an explicit per-provider override for
// Entra ID, which does not emit the claim at all), and it gates *JIT account
// creation* only. It never gates linking, because no value of that claim makes
// silent linking safe.
//
// # Not a way around anything
//
// An SSO login is a first factor. It does not satisfy a second one: if the
// account has TOTP enrolled the callback returns a `totp` challenge instead of
// a session, unless the provider is configured to be trusted for MFA *and* the
// ID token actually carries evidence (RFC 8176 `amr`, or a matching `acr`).
// Deactivation is re-checked at the point a session is minted, so a disabled
// account cannot come back in through the IdP.
package sso

import (
	"errors"
	"time"
)

// Caller-fault errors. The handler maps exactly these; anything else is an
// internal fault and is masked.
var (
	ErrProviderNotFound = errors.New("no single sign-on provider is configured for this workspace")
	ErrProviderDisabled = errors.New("single sign-on is not enabled for this workspace")

	// ErrStateInvalid covers an unknown, expired or already-used state. All
	// three are the same answer on purpose: the state is single-use, so a
	// replay is indistinguishable from a forgery and must not be told apart.
	ErrStateInvalid = errors.New("sign-in request is invalid or has expired")

	// ErrPendingInvalid is the same idea for the second-step tokens.
	ErrPendingInvalid = errors.New("sign-in challenge is invalid or has expired")

	// ErrAssertionInvalid is every way an ID token can fail verification:
	// signature, issuer, audience, expiry, nonce. One error for all of them
	// because the client must not learn which — the specific reason goes to the
	// log, where an operator can see it and an attacker cannot.
	ErrAssertionInvalid = errors.New("the identity provider's response could not be verified")

	// ErrExchangeFailed is the token endpoint refusing the authorization code:
	// a reused code, a mismatched redirect URI, a wrong client secret. Also
	// logged rather than echoed — the provider's error text names our
	// configuration.
	ErrExchangeFailed = errors.New("the identity provider rejected this sign-in")

	ErrEmailMissing     = errors.New("the identity provider returned no email address")
	ErrEmailNotVerified = errors.New("the identity provider did not verify this email address")
	ErrEmailAmbiguous   = errors.New("more than one account already uses this email address")

	// ErrLinkNotAllowed / ErrJITNotAllowed are policy refusals, not failures.
	ErrLinkNotAllowed = errors.New("an account already exists for this address and linking is disabled")
	ErrJITNotAllowed  = errors.New("no account exists for this address and automatic provisioning is disabled")

	ErrAccountInactive = errors.New("this account is deactivated")

	// ErrIdentityTaken means the local account is already bound to a different
	// subject at this provider.
	ErrIdentityTaken = errors.New("this account is already linked to a different single sign-on identity")

	ErrInvalidLocalCredential = errors.New("incorrect password")
	ErrSecondFactorRequired   = errors.New("two-factor code required")
	ErrSecondFactorInvalid    = errors.New("invalid two-factor code")

	// ErrEnforceNeedsIdentity is the lockout guard: an administrator may not
	// turn off password login for a workspace until they have proven, by
	// signing in through it once, that the provider actually works for them.
	ErrEnforceNeedsIdentity = errors.New("link your own single sign-on identity before requiring SSO for this workspace")

	// ErrNotConfigured is returned by every entry point when the deployment has
	// no SSO_SECRET_KEY: without it client secrets cannot be sealed or opened.
	ErrNotConfigured = errors.New("single sign-on is not configured on this deployment")
)

// Config is the deployment-level half of SSO. Everything workspace-specific
// lives in the sso_providers row instead.
type Config struct {
	// SecretKey is a 32-byte AES-256 key sealing client secrets at rest. Empty
	// disables the whole feature — see ErrNotConfigured.
	SecretKey []byte

	// AllowInsecureIssuer permits an http:// issuer and an issuer that resolves
	// to a loopback or private address. Off in production: with it on, a
	// workspace admin can point the "issuer" at 169.254.169.254 and read the
	// cloud metadata service through this process.
	AllowInsecureIssuer bool

	// HTTPTimeout bounds one call to the provider (discovery, JWKS, token).
	HTTPTimeout time.Duration

	// AuthRequestTTL is how long a started sign-in stays completable. It covers
	// a human typing a password and a TOTP code at the IdP, not a coffee break.
	AuthRequestTTL time.Duration

	// PendingTTL bounds the second step (2FA prompt, or the link challenge).
	PendingTTL time.Duration

	// JWKSCacheTTL is how long a fetched key set is served without revalidation.
	// Rotation does not wait for it: an unknown `kid` triggers a refetch,
	// throttled by jwksMinRefresh.
	JWKSCacheTTL time.Duration

	// ClockSkew is the tolerance applied to `exp` and `iat`.
	ClockSkew time.Duration
}

// Defaults fills every zero-valued field. Callers construct Config from
// environment variables, where "unset" is the common case.
func (c Config) Defaults() Config {
	if c.HTTPTimeout <= 0 {
		c.HTTPTimeout = 10 * time.Second
	}
	if c.AuthRequestTTL <= 0 {
		c.AuthRequestTTL = 10 * time.Minute
	}
	if c.PendingTTL <= 0 {
		c.PendingTTL = 5 * time.Minute
	}
	if c.JWKSCacheTTL <= 0 {
		c.JWKSCacheTTL = time.Hour
	}
	if c.ClockSkew <= 0 {
		c.ClockSkew = 2 * time.Minute
	}
	return c
}

// IsEnabled reports whether SSO should be constructed at all. It mirrors
// MinIOConfig.IsEnabled/MeiliConfig.IsEnabled so no caller tests a raw field.
func (c Config) IsEnabled() bool { return len(c.SecretKey) == aesKeyBytes }
