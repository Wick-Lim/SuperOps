package sso

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"strings"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	cryptopkg "github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Audit actions, in the "<resource>.<verb>" shape internal/audit uses. They
// live here rather than in internal/audit so that adding an SSO event does not
// mean editing a package this one only consumes.
const (
	actionSSOLogin       = "user.sso_login"
	actionSSOLoginFailed = "user.sso_login_failed"
	actionSSOProvisioned = "user.sso_provisioned"
	actionSSOLinked      = "user.sso_linked"
	actionSSORoleSynced  = "user.sso_role_synced"
	actionSSOConfigured  = "sso.configured"
	actionSSODeleted     = "sso.deleted"
	actionSSOEnforcement = "sso.enforcement_changed"
)

// Sessions is the subset of *auth.Service this package needs.
//
// An interface rather than the concrete type for one reason that matters: it
// states exactly how much of the credential system SSO is allowed to touch. It
// can mint a session, ask whether a second factor is enrolled, and spend one —
// it cannot enrol one, disable one, or read a password hash.
type Sessions interface {
	// IssueSSOSession mints a token pair for a user who authenticated through
	// an identity provider. Implementations must re-check that the account is
	// active.
	IssueSSOSession(ctx context.Context, userID, userAgent, ipAddress string) (*auth.TokenPair, error)
	// SecondFactorEnabled reports whether the account has TOTP enrolled. The
	// error is separate from the answer on purpose: a database fault must not
	// read as "no second factor".
	SecondFactorEnabled(ctx context.Context, userID string) (bool, error)
	// ConsumeSecondFactor spends a TOTP code or a backup code, once.
	ConsumeSecondFactor(ctx context.Context, userID, code string) (bool, error)
	// VerifyPassword checks the account's password.
	VerifyPassword(ctx context.Context, userID, password string) (bool, error)
}

// Service holds no authz.Checker: authorization stays in the handler, which is
// the convention internal/authz documents. Enforcement — the one membership
// question SSO raises — is asked by internal/auth at the password prompt, not
// here, because there is no SSO sign-in that enforcement would forbid.
type Service struct {
	repo     *Repository
	oidc     *Client
	sessions Sessions
	auditSvc *audit.Service
	cfg      Config
	logger   *slog.Logger
}

func NewService(repo *Repository, oidc *Client, sessions Sessions, auditSvc *audit.Service, cfg Config, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		repo:     repo,
		oidc:     oidc,
		sessions: sessions,
		auditSvc: auditSvc,
		cfg:      cfg.Defaults(),
		logger:   logger,
	}
}

func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.auditSvc == nil {
		return
	}
	s.auditSvc.Try(ctx, e)
}

// ---------------------------------------------------------------------------
// Sign-in
// ---------------------------------------------------------------------------

// PublicInfo is what an unauthenticated sign-in page may know about a
// workspace's provider: enough to render the button and to stop offering a
// password field that will not work.
type PublicInfo struct {
	WorkspaceID     string `json:"workspace_id"`
	Name            string `json:"name"`
	Enabled         bool   `json:"enabled"`
	PasswordEnabled bool   `json:"password_login_enabled"`
}

func (s *Service) PublicInfo(ctx context.Context, workspaceSlug string) (*PublicInfo, error) {
	p, err := s.repo.GetProviderByWorkspaceSlug(ctx, workspaceSlug)
	if err != nil {
		return nil, err
	}
	if p == nil || !p.Enabled {
		return nil, ErrProviderNotFound
	}
	return &PublicInfo{
		WorkspaceID:     p.WorkspaceID,
		Name:            p.Name,
		Enabled:         p.Enabled,
		PasswordEnabled: !p.Enforced,
	}, nil
}

// StartResult is the redirect a client must follow.
type StartResult struct {
	AuthorizationURL string `json:"authorization_url"`
	State            string `json:"state"`
	ExpiresIn        int    `json:"expires_in"`
}

// Start begins an Authorization Code + PKCE sign-in.
//
// state, nonce and the PKCE verifier are minted here and bound together in one
// row: state defends the callback against cross-site request forgery, nonce
// binds the ID token to this request, and the verifier binds the authorization
// code to this process. They are stored hashed (except the verifier, which has
// to be replayed verbatim) and consumed exactly once.
func (s *Service) Start(ctx context.Context, workspaceSlug string) (*StartResult, error) {
	if !s.cfg.IsEnabled() {
		return nil, ErrNotConfigured
	}

	p, err := s.repo.GetProviderByWorkspaceSlug(ctx, workspaceSlug)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProviderNotFound
	}
	if !p.Enabled {
		return nil, ErrProviderDisabled
	}

	md, err := s.oidc.Metadata(ctx, p.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover provider: %w", err)
	}

	state, err := cryptopkg.GenerateRandomToken(32)
	if err != nil {
		return nil, err
	}
	nonce, err := cryptopkg.GenerateRandomToken(32)
	if err != nil {
		return nil, err
	}
	verifier, err := newPKCE()
	if err != nil {
		return nil, err
	}

	if err := s.repo.CreateAuthRequest(ctx, &AuthRequest{
		StateHash:    cryptopkg.HashToken(state),
		ProviderID:   p.ID,
		NonceHash:    cryptopkg.HashToken(nonce),
		CodeVerifier: verifier.Verifier,
		ExpiresAt:    time.Now().Add(s.cfg.AuthRequestTTL),
	}); err != nil {
		return nil, err
	}

	authURL, err := authorizationURL(md.AuthorizationEndpoint, p, state, nonce, verifier.Challenge)
	if err != nil {
		return nil, err
	}

	return &StartResult{
		AuthorizationURL: authURL,
		State:            state,
		ExpiresIn:        int(s.cfg.AuthRequestTTL.Seconds()),
	}, nil
}

func authorizationURL(endpoint string, p *Provider, state, nonce, challenge string) (string, error) {
	u, err := url.Parse(endpoint)
	if err != nil {
		return "", fmt.Errorf("parse authorization endpoint: %w", err)
	}
	q := u.Query()
	q.Set("response_type", "code")
	q.Set("client_id", p.ClientID)
	// The redirect URI is configuration, never a request parameter: a
	// client-chosen one is an open redirector and, at providers that match it
	// loosely, an authorization-code exfiltration.
	q.Set("redirect_uri", p.RedirectURI)
	q.Set("scope", withOpenIDScope(p.Scopes))
	q.Set("state", state)
	q.Set("nonce", nonce)
	q.Set("code_challenge", challenge)
	q.Set("code_challenge_method", "S256")
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func withOpenIDScope(scopes string) string {
	fields := strings.Fields(scopes)
	for _, f := range fields {
		if f == "openid" {
			return strings.Join(fields, " ")
		}
	}
	return strings.Join(append([]string{"openid"}, fields...), " ")
}

// LoginResult is the outcome of a callback or of a completed second step.
//
// Exactly one of Tokens and Step is meaningful. A Step means the assertion
// verified but a session must not be minted yet, and the reason is either "this
// account has a second factor" or "this address already belongs to a local
// account".
type LoginResult struct {
	Tokens       *auth.TokenPair `json:"tokens,omitempty"`
	Step         string          `json:"step,omitempty"`
	PendingToken string          `json:"pending_token,omitempty"`
	ExpiresIn    int             `json:"expires_in,omitempty"`
	Email        string          `json:"email,omitempty"`
}

// Callback completes the redirect: it spends the state, redeems the code,
// verifies the ID token and then applies the linking rules.
func (s *Service) Callback(ctx context.Context, state, code, userAgent, ipAddress string) (*LoginResult, error) {
	if !s.cfg.IsEnabled() {
		return nil, ErrNotConfigured
	}
	if state == "" || code == "" {
		return nil, ErrStateInvalid
	}

	req, err := s.repo.ConsumeAuthRequest(ctx, cryptopkg.HashToken(state))
	if err != nil {
		return nil, err
	}
	if req == nil {
		// Unknown, expired, or already spent. A replayed callback lands here,
		// which is exactly what makes an ID token replay useless.
		return nil, ErrStateInvalid
	}

	p, err := s.repo.GetProviderByID(ctx, req.ProviderID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProviderNotFound
	}
	if !p.Enabled {
		return nil, ErrProviderDisabled
	}

	claims, err := s.assert(ctx, p, req, code)
	if err != nil {
		return nil, err
	}

	email := normalizeEmail(claims.Email)
	if email == "" {
		return nil, ErrEmailMissing
	}

	identity, err := s.repo.GetIdentity(ctx, p.ID, claims.Subject)
	if err != nil {
		return nil, err
	}
	if identity != nil {
		return s.loginKnownIdentity(ctx, p, identity, claims, email, userAgent, ipAddress)
	}

	// First time this subject has appeared. Everything below is the account
	// linking decision.
	existing, err := s.repo.FindLocalAccountByEmail(ctx, email)
	if err != nil {
		return nil, err
	}

	if existing != nil {
		return s.challengeLink(ctx, p, existing, claims, email, ipAddress)
	}
	return s.provision(ctx, p, claims, email, userAgent, ipAddress)
}

// assert redeems the code and verifies the resulting ID token.
func (s *Service) assert(ctx context.Context, p *Provider, req *AuthRequest, code string) (*IDClaims, error) {
	md, err := s.oidc.Metadata(ctx, p.Issuer)
	if err != nil {
		return nil, fmt.Errorf("discover provider: %w", err)
	}

	secret, err := p.ClientSecret(s.cfg.SecretKey)
	if err != nil {
		return nil, err
	}

	tokens, err := s.oidc.Exchange(ctx, ExchangeInput{
		TokenEndpoint: md.TokenEndpoint,
		ClientID:      p.ClientID,
		ClientSecret:  secret,
		Code:          code,
		RedirectURI:   p.RedirectURI,
		CodeVerifier:  req.CodeVerifier,
		AuthMethods:   md.TokenEndpointAuthMethodsSupported,
	})
	if err != nil {
		// The provider's error names our client id, our redirect URI and
		// sometimes our secret's state. It belongs in the operator's log, not
		// in an unauthenticated response.
		s.logger.Warn("sso token exchange failed",
			"error", err, "workspace_id", p.WorkspaceID, "issuer", p.Issuer)
		return nil, ErrExchangeFailed
	}

	claims, err := s.oidc.VerifyIDToken(ctx, md, tokens.IDToken, verifyOptions{
		Issuer:    p.Issuer,
		ClientID:  p.ClientID,
		NonceHash: req.NonceHash,
		Skew:      s.cfg.ClockSkew,
	})
	if err != nil {
		// Which check failed is a detail an attacker would use to iterate.
		s.logger.Warn("sso id_token rejected",
			"error", err, "workspace_id", p.WorkspaceID, "issuer", p.Issuer)
		s.record(ctx, audit.Entry{
			WorkspaceID:  p.WorkspaceID,
			Action:       actionSSOLoginFailed,
			ResourceType: "sso_provider",
			ResourceID:   p.ID,
			Metadata:     map[string]interface{}{"reason": "id_token_rejected"},
		})
		return nil, ErrAssertionInvalid
	}
	return claims, nil
}

// loginKnownIdentity is the steady state: this subject has signed in before.
func (s *Service) loginKnownIdentity(ctx context.Context, p *Provider, identity *Identity, claims *IDClaims, email, userAgent, ipAddress string) (*LoginResult, error) {
	role, asserted := resolveRole(p, claims)

	// Re-assert the membership. A JIT deployment treats the directory as the
	// source of truth, so a user removed from the workspace here but still in
	// the directory group comes back — deprovisioning belongs at the IdP. It is
	// gated on allow_jit so a deployment that wants local removals to stick can
	// have that instead.
	if p.AllowJIT {
		added, changedTo, err := s.repo.EnsureMembership(ctx, p.WorkspaceID, identity.UserID, role, asserted)
		if err != nil {
			return nil, err
		}
		if added || changedTo != "" {
			s.record(ctx, audit.Entry{
				WorkspaceID:  p.WorkspaceID,
				ActorID:      identity.UserID,
				Action:       actionSSORoleSynced,
				ResourceType: "workspace_member",
				ResourceID:   identity.UserID,
				IPAddress:    ipAddress,
				Metadata:     map[string]interface{}{"role": role, "readded": added},
			})
		}
	}

	if err := s.repo.TouchIdentity(ctx, identity.ID, email); err != nil {
		return nil, err
	}
	return s.finish(ctx, p, identity.UserID, claims, email, userAgent, ipAddress)
}

// challengeLink is the answer to the dangerous case: a verified assertion whose
// address already belongs to a local account that is not bound to this
// provider.
//
// It does not link. It returns a challenge that only the holder of the local
// account's password (and second factor) can complete. See the package comment
// for why the two cheaper answers — silent linking, and linking on a
// confirmation click — are both account takeover.
func (s *Service) challengeLink(ctx context.Context, p *Provider, existing *LocalAccount, claims *IDClaims, email, ipAddress string) (*LoginResult, error) {
	if !p.AllowLinking {
		return nil, ErrLinkNotAllowed
	}
	if !existing.IsActive {
		return nil, ErrAccountInactive
	}
	// An account with no password cannot complete a link challenge, and there
	// is nothing else to prove control with. This is reachable for an account
	// provisioned by a provider that was later deleted and re-created.
	if !existing.HasPassword {
		return nil, ErrLinkNotAllowed
	}

	token, err := s.newPending(ctx, p, existing.ID, PendingLink, claims.Subject, email)
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		WorkspaceID:  p.WorkspaceID,
		ActorID:      existing.ID,
		Action:       actionSSOLoginFailed,
		ResourceType: "user",
		ResourceID:   existing.ID,
		IPAddress:    ipAddress,
		Metadata:     map[string]interface{}{"reason": "link_challenge_issued", "email": email},
	})

	return &LoginResult{
		Step:         PendingLink,
		PendingToken: token,
		ExpiresIn:    int(s.cfg.PendingTTL.Seconds()),
		Email:        email,
	}, nil
}

// provision creates the account on first login.
//
// This is the only place email_verified is load-bearing. It gates account
// *creation*, where the address decides which company directory a new account
// is attributed to; it does not gate linking, because no value of the claim
// makes silent linking safe.
func (s *Service) provision(ctx context.Context, p *Provider, claims *IDClaims, email, userAgent, ipAddress string) (*LoginResult, error) {
	if !p.AllowJIT {
		return nil, ErrJITNotAllowed
	}
	if p.RequireVerifiedEmail && !claims.EmailVerified.Value {
		return nil, ErrEmailNotVerified
	}

	role, _ := resolveRole(p, claims)
	userID, err := s.repo.ProvisionUser(ctx, ProvisionInput{
		ProviderID:        p.ID,
		WorkspaceID:       p.WorkspaceID,
		Subject:           claims.Subject,
		Email:             email,
		FullName:          displayName(claims),
		PreferredUsername: claims.PreferredUsername,
		Role:              role,
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		WorkspaceID:  p.WorkspaceID,
		ActorID:      userID,
		Action:       actionSSOProvisioned,
		ResourceType: "user",
		ResourceID:   userID,
		IPAddress:    ipAddress,
		Metadata:     map[string]interface{}{"email": email, "role": role, "subject": claims.Subject},
	})

	return s.finish(ctx, p, userID, claims, email, userAgent, ipAddress)
}

// finish is the single point where an SSO assertion becomes a session, which is
// why both of the things SSO must not bypass are checked here.
func (s *Service) finish(ctx context.Context, p *Provider, userID string, claims *IDClaims, email, userAgent, ipAddress string) (*LoginResult, error) {
	active, err := s.repo.IsActive(ctx, userID)
	if err != nil {
		return nil, err
	}
	if !active {
		s.record(ctx, audit.Entry{
			WorkspaceID:  p.WorkspaceID,
			ActorID:      userID,
			Action:       actionSSOLoginFailed,
			ResourceType: "user",
			ResourceID:   userID,
			IPAddress:    ipAddress,
			Metadata:     map[string]interface{}{"reason": "account_deactivated"},
		})
		return nil, ErrAccountInactive
	}

	// A federated first factor is still one factor. If the account has TOTP
	// enrolled, SSO stops here and asks for it — otherwise "sign in with the
	// IdP" would be a documented way to drop a factor the user chose to add.
	needsSecondFactor, err := s.sessions.SecondFactorEnabled(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("load second factor state: %w", err)
	}
	if needsSecondFactor && idpSatisfiedMFA(claims, p.TrustIDPMFA, p.RequiredACR) {
		needsSecondFactor = false
	}
	if needsSecondFactor {
		token, err := s.newPending(ctx, p, userID, PendingTOTP, claims.Subject, email)
		if err != nil {
			return nil, err
		}
		return &LoginResult{
			Step:         PendingTOTP,
			PendingToken: token,
			ExpiresIn:    int(s.cfg.PendingTTL.Seconds()),
			Email:        email,
		}, nil
	}

	return s.issue(ctx, p, userID, userAgent, ipAddress, false)
}

func (s *Service) issue(ctx context.Context, p *Provider, userID, userAgent, ipAddress string, secondFactor bool) (*LoginResult, error) {
	tokens, err := s.sessions.IssueSSOSession(ctx, userID, userAgent, ipAddress)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		WorkspaceID:  p.WorkspaceID,
		ActorID:      userID,
		Action:       actionSSOLogin,
		ResourceType: "user",
		ResourceID:   userID,
		IPAddress:    ipAddress,
		Metadata:     map[string]interface{}{"provider": p.Name, "issuer": p.Issuer, "second_factor": secondFactor},
	})
	return &LoginResult{Tokens: tokens}, nil
}

func (s *Service) newPending(ctx context.Context, p *Provider, userID, kind, subject, email string) (string, error) {
	token, err := cryptopkg.GenerateRandomToken(32)
	if err != nil {
		return "", err
	}
	if err := s.repo.CreatePendingLogin(ctx, &PendingLogin{
		ProviderID: p.ID,
		UserID:     userID,
		Kind:       kind,
		Subject:    subject,
		Email:      email,
		ExpiresAt:  time.Now().Add(s.cfg.PendingTTL),
	}, cryptopkg.HashToken(token)); err != nil {
		return "", err
	}
	return token, nil
}

// CompleteSecondFactor finishes a login that stopped at the TOTP prompt.
func (s *Service) CompleteSecondFactor(ctx context.Context, pendingToken, code, userAgent, ipAddress string) (*LoginResult, error) {
	pending, p, err := s.consumePending(ctx, pendingToken, PendingTOTP)
	if err != nil {
		return nil, err
	}

	ok, err := s.sessions.ConsumeSecondFactor(ctx, pending.UserID, code)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.record(ctx, audit.Entry{
			WorkspaceID:  p.WorkspaceID,
			ActorID:      pending.UserID,
			Action:       actionSSOLoginFailed,
			ResourceType: "user",
			ResourceID:   pending.UserID,
			IPAddress:    ipAddress,
			Metadata:     map[string]interface{}{"reason": "invalid_totp"},
		})
		// The pending row is already spent, so a wrong code costs a full
		// round trip through the IdP. That is deliberate: it makes the prompt
		// unusable as a TOTP oracle.
		return nil, ErrSecondFactorInvalid
	}

	active, err := s.repo.IsActive(ctx, pending.UserID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrAccountInactive
	}
	return s.issue(ctx, p, pending.UserID, userAgent, ipAddress, true)
}

// CompleteLink binds an SSO identity to an existing local account, on proof of
// that account's password and — if enrolled — its second factor.
//
// The proof is the whole mechanism. Anyone can reach this endpoint with a
// pending token, because anyone who can authenticate at the IdP can obtain one;
// only the person who already controls the local account can finish.
func (s *Service) CompleteLink(ctx context.Context, pendingToken, password, code, userAgent, ipAddress string) (*LoginResult, error) {
	pending, p, err := s.consumePending(ctx, pendingToken, PendingLink)
	if err != nil {
		return nil, err
	}
	// Re-read the policy: an administrator may have turned linking off between
	// the challenge and now.
	if !p.AllowLinking {
		return nil, ErrLinkNotAllowed
	}

	ok, err := s.sessions.VerifyPassword(ctx, pending.UserID, password)
	if err != nil {
		return nil, err
	}
	if !ok {
		s.record(ctx, audit.Entry{
			WorkspaceID:  p.WorkspaceID,
			ActorID:      pending.UserID,
			Action:       actionSSOLoginFailed,
			ResourceType: "user",
			ResourceID:   pending.UserID,
			IPAddress:    ipAddress,
			Metadata:     map[string]interface{}{"reason": "link_password_rejected"},
		})
		return nil, ErrInvalidLocalCredential
	}

	needsSecondFactor, err := s.sessions.SecondFactorEnabled(ctx, pending.UserID)
	if err != nil {
		return nil, fmt.Errorf("load second factor state: %w", err)
	}
	if needsSecondFactor {
		if code == "" {
			// The pending row is spent, so this is not a retry-friendly answer.
			// It is still the right one: linking must not be completable
			// without every factor the account carries.
			return nil, ErrSecondFactorRequired
		}
		ok, err := s.sessions.ConsumeSecondFactor(ctx, pending.UserID, code)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, ErrSecondFactorInvalid
		}
	}

	active, err := s.repo.IsActive(ctx, pending.UserID)
	if err != nil {
		return nil, err
	}
	if !active {
		return nil, ErrAccountInactive
	}

	if err := s.repo.LinkIdentity(ctx, p.ID, pending.UserID, pending.Subject, pending.Email); err != nil {
		return nil, err
	}
	if p.AllowJIT {
		if _, _, err := s.repo.EnsureMembership(ctx, p.WorkspaceID, pending.UserID, p.DefaultRole, false); err != nil {
			return nil, err
		}
	}

	s.record(ctx, audit.Entry{
		WorkspaceID:  p.WorkspaceID,
		ActorID:      pending.UserID,
		Action:       actionSSOLinked,
		ResourceType: "user",
		ResourceID:   pending.UserID,
		IPAddress:    ipAddress,
		Metadata:     map[string]interface{}{"subject": pending.Subject, "email": pending.Email},
	})

	return s.issue(ctx, p, pending.UserID, userAgent, ipAddress, needsSecondFactor)
}

func (s *Service) consumePending(ctx context.Context, token, kind string) (*PendingLogin, *Provider, error) {
	if !s.cfg.IsEnabled() {
		return nil, nil, ErrNotConfigured
	}
	if token == "" {
		return nil, nil, ErrPendingInvalid
	}
	pending, err := s.repo.ConsumePendingLogin(ctx, cryptopkg.HashToken(token), kind)
	if err != nil {
		return nil, nil, err
	}
	if pending == nil {
		return nil, nil, ErrPendingInvalid
	}
	p, err := s.repo.GetProviderByID(ctx, pending.ProviderID)
	if err != nil {
		return nil, nil, err
	}
	if p == nil {
		return nil, nil, ErrProviderNotFound
	}
	if !p.Enabled {
		return nil, nil, ErrProviderDisabled
	}
	return pending, p, nil
}

// resolveRole maps the provider's assertion onto a workspace role.
//
// The second return value distinguishes "the provider said so" from "we used
// the default". Only an asserted role is allowed to change an existing member's
// role on a later login; a default must not silently demote somebody an
// administrator promoted by hand.
func resolveRole(p *Provider, claims *IDClaims) (string, bool) {
	fallback := p.DefaultRole
	if fallback == "" {
		fallback = authz.RoleMember
	}
	if p.RoleClaim == "" || len(p.RoleMapping) == 0 {
		return fallback, false
	}

	best := ""
	for _, value := range claims.Claim(p.RoleClaim) {
		mapped, ok := p.RoleMapping[value]
		if !ok {
			continue
		}
		if !isAssignableRole(mapped) {
			continue
		}
		if best == "" || rolePrivilege(mapped) > rolePrivilege(best) {
			best = mapped
		}
	}
	if best == "" {
		return fallback, false
	}
	return best, true
}

// isAssignableRole is the guard that keeps 'owner' out of every SSO path.
// Ownership moves through POST /workspaces/{id}/transfer-ownership and nowhere
// else; an IdP group named "owner" must not be a way to take a workspace.
func isAssignableRole(role string) bool {
	switch role {
	case authz.RoleAdmin, authz.RoleMember, authz.RoleGuest:
		return true
	default:
		return false
	}
}

func rolePrivilege(role string) int {
	switch role {
	case authz.RoleAdmin:
		return 3
	case authz.RoleMember:
		return 2
	case authz.RoleGuest:
		return 1
	default:
		return 0
	}
}

func displayName(claims *IDClaims) string {
	if claims.Name != "" {
		return claims.Name
	}
	joined := strings.TrimSpace(claims.GivenName + " " + claims.FamilyName)
	if joined != "" {
		return joined
	}
	return claims.PreferredUsername
}

// ---------------------------------------------------------------------------
// Administration
// ---------------------------------------------------------------------------

// SaveInput is the API-facing shape of a provider configuration.
type SaveInput struct {
	Name        string `json:"name"`
	Issuer      string `json:"issuer"`
	ClientID    string `json:"client_id"`
	RedirectURI string `json:"redirect_uri"`
	Scopes      string `json:"scopes"`

	// ClientSecret nil leaves the stored secret alone, "" clears it (a public
	// client), any other value replaces it. It is write-only: no response ever
	// contains it.
	ClientSecret *string `json:"client_secret"`

	Enabled                 *bool `json:"enabled"`
	AllowOwnerPasswordLogin *bool `json:"allow_owner_password_login"`
	AllowJIT                *bool `json:"allow_jit"`
	AllowLinking            *bool `json:"allow_linking"`
	RequireVerifiedEmail    *bool `json:"require_verified_email"`
	TrustIDPMFA             *bool `json:"trust_idp_mfa"`

	RequiredACR string            `json:"required_acr"`
	DefaultRole string            `json:"default_role"`
	RoleClaim   string            `json:"role_claim"`
	RoleMapping map[string]string `json:"role_mapping"`
}

func (s *Service) GetProvider(ctx context.Context, workspaceID string) (*Provider, error) {
	p, err := s.repo.GetProviderByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProviderNotFound
	}
	return p, nil
}

// SaveProvider writes a workspace's configuration.
//
// Enabling runs discovery first. §3c of the roadmap applies to this as much as
// to a mail transport: a misconfiguration that is only discovered at first use
// is discovered by a user who cannot sign in, with nothing in the log to point
// at. Here the administrator gets the provider's actual error, at the moment
// they made the mistake.
func (s *Service) SaveProvider(ctx context.Context, workspaceID, actorID string, in SaveInput, ipAddress string) (*Provider, error) {
	if !s.cfg.IsEnabled() {
		return nil, ErrNotConfigured
	}

	current, err := s.repo.GetProviderByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}

	save := SaveProviderInput{
		WorkspaceID:             workspaceID,
		Name:                    strings.TrimSpace(in.Name),
		Issuer:                  strings.TrimRight(strings.TrimSpace(in.Issuer), "/"),
		ClientID:                strings.TrimSpace(in.ClientID),
		RedirectURI:             strings.TrimSpace(in.RedirectURI),
		Scopes:                  withOpenIDScope(in.Scopes),
		RequiredACR:             strings.TrimSpace(in.RequiredACR),
		DefaultRole:             strings.TrimSpace(in.DefaultRole),
		RoleClaim:               strings.TrimSpace(in.RoleClaim),
		RoleMapping:             in.RoleMapping,
		Enabled:                 boolOr(in.Enabled, current != nil && current.Enabled),
		AllowOwnerPasswordLogin: boolOr(in.AllowOwnerPasswordLogin, current == nil || current.AllowOwnerPasswordLogin),
		AllowJIT:                boolOr(in.AllowJIT, current == nil || current.AllowJIT),
		AllowLinking:            boolOr(in.AllowLinking, current == nil || current.AllowLinking),
		RequireVerifiedEmail:    boolOr(in.RequireVerifiedEmail, current == nil || current.RequireVerifiedEmail),
		TrustIDPMFA:             boolOr(in.TrustIDPMFA, current != nil && current.TrustIDPMFA),
	}
	if save.DefaultRole == "" {
		save.DefaultRole = authz.RoleMember
	}

	if appErr := validateSaveInput(save); appErr != nil {
		return nil, appErr
	}

	clearSecret := false
	switch {
	case in.ClientSecret == nil:
		// Keep whatever is stored.
	case strings.TrimSpace(*in.ClientSecret) == "":
		clearSecret = true
	default:
		sealed, err := sealSecret(s.cfg.SecretKey, *in.ClientSecret)
		if err != nil {
			return nil, err
		}
		save.ClientSecretEnc = sealed
	}

	if save.Enabled {
		if _, err := s.oidc.Metadata(ctx, save.Issuer); err != nil {
			return nil, httputil.NewBadRequest(fmt.Sprintf("the identity provider could not be reached or is not an OIDC issuer: %v", err))
		}
	}

	saved, err := s.repo.SaveProvider(ctx, save)
	if err != nil {
		return nil, err
	}
	if saved == nil {
		return nil, ErrProviderNotFound
	}
	if clearSecret {
		if err := s.repo.ClearClientSecret(ctx, saved.ID); err != nil {
			return nil, err
		}
		saved.clientSecretEnc = nil
	}

	s.record(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       actionSSOConfigured,
		ResourceType: "sso_provider",
		ResourceID:   saved.ID,
		IPAddress:    ipAddress,
		Metadata: map[string]interface{}{
			"issuer": saved.Issuer, "enabled": saved.Enabled, "enforced": saved.Enforced,
			"allow_jit": saved.AllowJIT, "allow_linking": saved.AllowLinking,
			"require_verified_email": saved.RequireVerifiedEmail,
		},
	})
	return saved, nil
}

func boolOr(v *bool, fallback bool) bool {
	if v == nil {
		return fallback
	}
	return *v
}

func validateSaveInput(in SaveProviderInput) *httputil.AppError {
	if in.Issuer == "" {
		return httputil.NewBadRequest("issuer is required")
	}
	if in.ClientID == "" {
		return httputil.NewBadRequest("client_id is required")
	}
	if in.RedirectURI == "" {
		return httputil.NewBadRequest("redirect_uri is required")
	}
	u, err := url.Parse(in.RedirectURI)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return httputil.NewBadRequest("redirect_uri must be an absolute http(s) URL")
	}
	if !isAssignableRole(in.DefaultRole) {
		return httputil.NewBadRequest("default_role must be admin, member or guest")
	}
	for value, role := range in.RoleMapping {
		if !isAssignableRole(role) {
			return httputil.NewBadRequest(fmt.Sprintf("role_mapping[%q] must be admin, member or guest", value))
		}
	}
	if in.RoleClaim != "" && len(in.RoleMapping) == 0 {
		return httputil.NewBadRequest("role_claim is set but role_mapping is empty, so no role could ever be asserted")
	}
	return nil
}

func (s *Service) DeleteProvider(ctx context.Context, workspaceID, actorID, ipAddress string) error {
	deleted, err := s.repo.DeleteProvider(ctx, workspaceID)
	if err != nil {
		return err
	}
	if !deleted {
		return ErrProviderNotFound
	}
	s.record(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       actionSSODeleted,
		ResourceType: "sso_provider",
		ResourceID:   workspaceID,
		IPAddress:    ipAddress,
	})
	return nil
}

// SetEnforced turns password login off (or back on) for a workspace.
//
// The lockout guard: an administrator may only turn enforcement ON once their
// own account is bound to this provider. That single check is what makes the
// feature safe to ship — it is not possible to disable the only way you can
// sign in without having demonstrated, end to end, that the other way works for
// you specifically.
//
// The second guard lives in the data model: sso_providers.
// allow_owner_password_login (default true) keeps workspace owners able to sign
// in with a password even under enforcement, so a provider that breaks after
// the fact — an expired secret, a rotated client, an IdP outage — still leaves
// somebody able to reach the switch and turn it off.
func (s *Service) SetEnforced(ctx context.Context, workspaceID, actorID string, enforced bool, ipAddress string) (*Provider, error) {
	p, err := s.repo.GetProviderByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProviderNotFound
	}

	if enforced {
		if !p.Enabled {
			return nil, ErrProviderDisabled
		}
		identity, err := s.repo.GetIdentityByUser(ctx, p.ID, actorID)
		if err != nil {
			return nil, err
		}
		if identity == nil {
			return nil, ErrEnforceNeedsIdentity
		}
	}

	if err := s.repo.SetEnforced(ctx, p.ID, enforced); err != nil {
		return nil, err
	}
	p.Enforced = enforced

	s.record(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      actorID,
		Action:       actionSSOEnforcement,
		ResourceType: "sso_provider",
		ResourceID:   p.ID,
		IPAddress:    ipAddress,
		Metadata: map[string]interface{}{
			"enforced":                   enforced,
			"allow_owner_password_login": p.AllowOwnerPasswordLogin,
		},
	})
	return p, nil
}

// VerifyResult is what an operator gets from the "does this actually work"
// button. Roadmap §3c rule 3: give the operator a way to verify, and report the
// real error rather than a generic failure.
type VerifyResult struct {
	Issuer                string   `json:"issuer"`
	AuthorizationEndpoint string   `json:"authorization_endpoint"`
	TokenEndpoint         string   `json:"token_endpoint"`
	JWKSURI               string   `json:"jwks_uri"`
	SigningKeys           int      `json:"signing_keys"`
	SupportsS256          bool     `json:"supports_pkce_s256"`
	Warnings              []string `json:"warnings"`
}

func (s *Service) Verify(ctx context.Context, workspaceID string) (*VerifyResult, error) {
	p, err := s.repo.GetProviderByWorkspace(ctx, workspaceID)
	if err != nil {
		return nil, err
	}
	if p == nil {
		return nil, ErrProviderNotFound
	}

	md, err := s.oidc.Metadata(ctx, p.Issuer)
	if err != nil {
		return nil, httputil.NewBadRequest(fmt.Sprintf("discovery failed: %v", err))
	}
	keys, err := s.oidc.fetchKeys(ctx, md.JWKSURI)
	if err != nil {
		return nil, httputil.NewBadRequest(fmt.Sprintf("jwks fetch failed: %v", err))
	}

	out := &VerifyResult{
		Issuer:                md.Issuer,
		AuthorizationEndpoint: md.AuthorizationEndpoint,
		TokenEndpoint:         md.TokenEndpoint,
		JWKSURI:               md.JWKSURI,
		SigningKeys:           len(keys),
		Warnings:              []string{},
	}
	for _, m := range md.CodeChallengeMethodsSupported {
		if m == "S256" {
			out.SupportsS256 = true
		}
	}
	if len(md.CodeChallengeMethodsSupported) > 0 && !out.SupportsS256 {
		// Not fatal — the parameters are still sent — but a provider that
		// ignores PKCE gives none of its protection, and an operator should
		// know that before they rely on it.
		out.Warnings = append(out.Warnings, "the provider does not advertise PKCE S256 support")
	}
	if !p.RequireVerifiedEmail {
		out.Warnings = append(out.Warnings,
			"require_verified_email is off: accounts will be created for addresses this provider has not verified")
	}
	if p.HasClientSecret() {
		if _, err := p.ClientSecret(s.cfg.SecretKey); err != nil {
			out.Warnings = append(out.Warnings, "the stored client secret cannot be decrypted; re-enter it")
		}
	}
	return out, nil
}
