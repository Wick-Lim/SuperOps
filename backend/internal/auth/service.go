package auth

import (
	"context"
	"errors"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/audit"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// Caller-fault errors. Handlers map exactly these onto 4xx responses; anything
// else coming out of the service is an internal fault and is masked.
var (
	// ErrInvalidCredentials is the single answer to an unknown email, a wrong
	// password and a deactivated account alike, so that login cannot be used
	// to enumerate accounts or probe account state.
	ErrInvalidCredentials = errors.New("invalid credentials")
	ErrInvalidTOTPCode    = errors.New("invalid two-factor code")

	ErrInvalidRefreshToken = errors.New("invalid or expired refresh token")

	ErrInviteInvalid       = errors.New("invite is invalid or has already been used")
	ErrInviteExpired       = errors.New("invite has expired")
	ErrInviteWrongAccount  = errors.New("this invite belongs to a different account")
	ErrUsernameTaken       = errors.New("username already taken")
	ErrSignupFieldsMissing = errors.New("username and password are required")

	ErrPasswordTooShort = fmt.Errorf("password must be at least %d characters", minPasswordLen)
	ErrPasswordTooLong  = fmt.Errorf("password must be at most %d bytes", maxPasswordBytes)
	ErrCurrentPassword  = errors.New("current password is incorrect")

	// ErrReauthRequired is returned by operations that a stolen access token
	// alone must not be able to perform.
	ErrReauthRequired = errors.New("current password or a valid two-factor code is required")

	// ErrSSORequired means the credentials were correct but the account belongs
	// to a workspace that requires single sign-on. It is deliberately raised
	// only AFTER the password (and second factor) have been verified: raised
	// earlier it would tell an unauthenticated prober which addresses belong to
	// which organization.
	ErrSSORequired = errors.New("this organization requires single sign-on")
)

const (
	minPasswordLen = 8
	// bcrypt hashes only the first 72 bytes and silently discards the rest, so
	// accepting a longer password would mean accepting one whose tail is
	// meaningless. Reject it instead of pretending it counted.
	maxPasswordBytes = 72
)

// dummyPasswordHash is a real bcrypt cost-12 hash of a value nobody knows. It
// is compared against when the submitted email has no account, so that a login
// attempt costs the same whether or not the account exists.
const dummyPasswordHash = "$2a$12$u8m7xAI0Pp1kW0ZJq.n4ee1EMs4Qv.Wg6VkqBf2iiFNhQ/u9oYOoi"

func validatePassword(pw string) error {
	if len(pw) < minPasswordLen {
		return ErrPasswordTooShort
	}
	if len(pw) > maxPasswordBytes {
		return ErrPasswordTooLong
	}
	return nil
}

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	if host == "::1" {
		return "127.0.0.1"
	}
	return host
}

type Service struct {
	repo       *Repository
	userRepo   *user.Repository
	pool       *pgxpool.Pool
	jwtMgr     *JWTManager
	refreshTTL time.Duration
	auditSvc   *audit.Service
	// az answers whether single sign-on enforcement forbids a password login.
	// nil means no enforcement is possible (no SSO wired), which is the right
	// behaviour for a deployment without it and is what the unit tests use.
	az *authz.Checker
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

// Option configures optional collaborators. It exists so that adding one does
// not renumber the positional arguments of every existing call site — the
// composition root passes what a deployment has, and a test passes nothing.
type Option func(*Service)

// WithAuthz supplies the membership checker used for single sign-on
// enforcement. Without it, PasswordLoginAllowed is never consulted and password
// login behaves exactly as it did before SSO existed.
func WithAuthz(az *authz.Checker) Option {
	return func(s *Service) { s.az = az }
}

func NewService(repo *Repository, userRepo *user.Repository, pool *pgxpool.Pool, jwtMgr *JWTManager, refreshTTL time.Duration, auditSvc *audit.Service, opts ...Option) *Service {
	s := &Service{
		repo:       repo,
		userRepo:   userRepo,
		pool:       pool,
		jwtMgr:     jwtMgr,
		refreshTTL: refreshTTL,
		auditSvc:   auditSvc,
	}
	for _, opt := range opts {
		opt(s)
	}
	return s
}

// requirePasswordLoginAllowed is the enforcement gate.
//
// A session is global and enforcement is per-workspace, so the question is
// "does this person belong to any workspace that has switched password login
// off" — answered by internal/authz, never here. A nil checker means no SSO is
// configured at all.
func (s *Service) requirePasswordLoginAllowed(ctx context.Context, userID string) error {
	if s.az == nil {
		return nil
	}
	allowed, err := s.az.PasswordLoginAllowed(ctx, userID)
	if err != nil {
		// Fail closed: an unreachable database must not be a way to sign in
		// with a password the organization has disabled.
		return fmt.Errorf("check sso enforcement: %w", err)
	}
	if !allowed {
		return ErrSSORequired
	}
	return nil
}

// record writes an audit entry if auditing is configured. Failures are logged
// by the audit service, never propagated: the audited action already happened.
func (s *Service) record(ctx context.Context, e audit.Entry) {
	if s.auditSvc == nil {
		return
	}
	s.auditSvc.Try(ctx, e)
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

func (s *Service) Login(ctx context.Context, input LoginInput, userAgent, ipAddress string) (*TokenPair, error) {
	ip := extractIP(ipAddress)

	u, err := s.userRepo.GetByEmail(ctx, input.Email)
	if err != nil {
		return nil, fmt.Errorf("look up user: %w", err)
	}

	// Always pay for exactly one bcrypt comparison, and answer with the same
	// error in every failure case: an unknown email, a wrong password and a
	// deactivated account must be indistinguishable in both body and timing.
	hash := dummyPasswordHash
	if u != nil && u.PasswordHash != "" {
		hash = u.PasswordHash
	}
	passwordOK := crypto.CheckPassword(input.Password, hash)

	if u == nil || !u.IsActive || u.PasswordHash == "" || !passwordOK {
		actor := ""
		if u != nil {
			actor = u.ID
		}
		s.record(ctx, audit.Entry{
			ActorID:      actor,
			Action:       audit.ActionLoginFailed,
			ResourceType: "user",
			ResourceID:   actor,
			IPAddress:    ip,
			Metadata:     map[string]interface{}{"email": input.Email, "reason": loginFailureReason(u, passwordOK)},
		})
		return nil, ErrInvalidCredentials
	}

	// Second factor, if enabled. A failure to read the 2FA state must fail
	// closed: silently treating a transient DB fault as "2FA is off" would
	// downgrade every protected account to password-only.
	var secret string
	var totpEnabled bool
	if err := s.pool.QueryRow(ctx,
		`SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, u.ID,
	).Scan(&secret, &totpEnabled); err != nil {
		return nil, fmt.Errorf("load totp state: %w", err)
	}
	if totpEnabled {
		if input.TOTPCode == "" {
			return nil, ErrTOTPRequired
		}
		if !s.verifyTOTPOrBackup(ctx, u.ID, secret, input.TOTPCode) {
			s.record(ctx, audit.Entry{
				ActorID:      u.ID,
				Action:       audit.ActionLoginFailed,
				ResourceType: "user",
				ResourceID:   u.ID,
				IPAddress:    ip,
				Metadata:     map[string]interface{}{"email": input.Email, "reason": "invalid_totp"},
			})
			return nil, ErrInvalidTOTPCode
		}
	}

	// Everything about the credentials checked out. The last question is
	// whether this account is allowed to use them at all.
	if err := s.requirePasswordLoginAllowed(ctx, u.ID); err != nil {
		if errors.Is(err, ErrSSORequired) {
			s.record(ctx, audit.Entry{
				ActorID:      u.ID,
				Action:       audit.ActionLoginFailed,
				ResourceType: "user",
				ResourceID:   u.ID,
				IPAddress:    ip,
				Metadata:     map[string]interface{}{"email": input.Email, "reason": "sso_required"},
			})
		}
		return nil, err
	}

	tokens, err := s.issueTokens(ctx, u.ID, userAgent, ipAddress, MethodPassword)
	if err != nil {
		return nil, err
	}
	s.record(ctx, audit.Entry{
		ActorID:      u.ID,
		Action:       audit.ActionLogin,
		ResourceType: "user",
		ResourceID:   u.ID,
		IPAddress:    ip,
		Metadata:     map[string]interface{}{"totp": totpEnabled},
	})
	return tokens, nil
}

// loginFailureReason classifies a failed login for the audit trail only. It is
// never surfaced to the client.
func loginFailureReason(u *user.User, passwordOK bool) string {
	switch {
	case u == nil:
		return "unknown_email"
	case !u.IsActive:
		return "account_deactivated"
	case u.PasswordHash == "":
		return "no_password_set"
	case !passwordOK:
		return "bad_password"
	default:
		return "unknown"
	}
}

func (s *Service) RefreshTokens(ctx context.Context, refreshToken, userAgent, ipAddress string) (*TokenPair, error) {
	session, err := s.repo.GetSessionByToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, ErrInvalidRefreshToken
	}
	if time.Now().After(session.ExpiresAt) {
		if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
			return nil, err
		}
		return nil, ErrInvalidRefreshToken
	}

	// Deactivation has to cut the refresh chain now, not whenever the session
	// happens to expire — otherwise a disabled account keeps minting access
	// tokens for the full refresh TTL.
	var isActive bool
	err = s.pool.QueryRow(ctx, `SELECT is_active FROM users WHERE id = $1`, session.UserID).Scan(&isActive)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInvalidRefreshToken
	}
	if err != nil {
		return nil, fmt.Errorf("load user for refresh: %w", err)
	}
	if !isActive {
		if err := s.repo.DeleteUserSessions(ctx, session.UserID); err != nil {
			return nil, err
		}
		return nil, ErrInvalidRefreshToken
	}

	// Enforcement turned on after this session was minted must bite now, not in
	// thirty days when the refresh token would have expired on its own. Only a
	// password session is re-evaluated: an SSO session is exactly what
	// enforcement is asking for.
	if session.AuthMethod != MethodSSO {
		if err := s.requirePasswordLoginAllowed(ctx, session.UserID); err != nil {
			return nil, err
		}
	}

	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return nil, err
	}

	return s.issueTokens(ctx, session.UserID, userAgent, ipAddress, session.AuthMethod)
}

func (s *Service) Logout(ctx context.Context, refreshToken, ipAddress string) error {
	session, err := s.repo.GetSessionByToken(ctx, refreshToken)
	if err != nil {
		return err
	}
	if session == nil {
		return nil
	}
	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return err
	}
	s.record(ctx, audit.Entry{
		ActorID:      session.UserID,
		Action:       audit.ActionLogout,
		ResourceType: "user",
		ResourceID:   session.UserID,
		IPAddress:    extractIP(ipAddress),
	})
	return nil
}

// AcceptInviteInput carries everything the accept-invite endpoint knows.
type AcceptInviteInput struct {
	Token    string
	Username string
	Password string
	FullName string
	// BearerToken is the caller's access token when the accept request came
	// from an already-signed-in client; empty for an anonymous accept.
	BearerToken string
	UserAgent   string
	IPAddress   string
}

// AcceptInvite consumes an invite token and returns a session for the invitee.
//
// Two shapes: if no account exists for the invited address the invite is a
// signup and the account is created; if one does exist the invite is a join
// into a second workspace, which requires proof that the caller *is* that
// account (their access token or their password) — an invite mailed to an
// address must never hand a stranger a session for it.
func (s *Service) AcceptInvite(ctx context.Context, in AcceptInviteInput) (*TokenPair, error) {
	var inviteID, email, workspaceID, role, status string
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, workspace_id, role, status, expires_at FROM invitations WHERE token_hash = $1`,
		crypto.HashToken(in.Token),
	).Scan(&inviteID, &email, &workspaceID, &role, &status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load invitation: %w", err)
	}
	if status != "pending" {
		return nil, ErrInviteInvalid
	}
	if time.Now().After(expiresAt) {
		if _, err := s.pool.Exec(ctx,
			`UPDATE invitations SET status = 'expired' WHERE id = $1 AND status = 'pending'`, inviteID); err != nil {
			return nil, fmt.Errorf("expire invitation: %w", err)
		}
		return nil, ErrInviteExpired
	}

	// Refuse before anything is consumed. A workspace that requires single
	// sign-on cannot be joined by password: the session this would mint is one
	// RefreshTokens would immediately refuse, and the invite would have been
	// burned for nothing. With enforcement on, provisioning happens through the
	// provider instead.
	if s.az != nil {
		enforced, err := s.az.SSOEnforced(ctx, workspaceID)
		if err != nil {
			return nil, fmt.Errorf("check sso enforcement: %w", err)
		}
		if enforced {
			return nil, ErrSSORequired
		}
	}

	existing, err := s.userRepo.GetByEmail(ctx, email)
	if err != nil {
		return nil, fmt.Errorf("look up invited user: %w", err)
	}

	var userID, newUserHash string
	if existing != nil {
		if err := s.authorizeInvitee(existing, in); err != nil {
			return nil, err
		}
		userID = existing.ID
	} else {
		if in.Username == "" || in.Password == "" {
			return nil, ErrSignupFieldsMissing
		}
		if err := validatePassword(in.Password); err != nil {
			return nil, err
		}
		taken, err := s.userRepo.GetByUsername(ctx, in.Username)
		if err != nil {
			return nil, fmt.Errorf("check username: %w", err)
		}
		if taken != nil {
			return nil, ErrUsernameTaken
		}
		if newUserHash, err = crypto.HashPassword(in.Password); err != nil {
			return nil, err
		}
		userID = uuid.NewString()
	}

	// Create the account (when it is a signup), add it to the workspace +
	// #general, and consume the invite atomically: a partial failure must
	// never leave an orphaned user or a burned invite.
	err = database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if existing == nil {
			if _, err := tx.Exec(ctx,
				`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
				 VALUES ($1, $2, $3, $4, $5, true)`,
				userID, email, in.Username, in.FullName, newUserHash,
			); err != nil {
				return fmt.Errorf("create user: %w", err)
			}
		}

		if _, err := tx.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)
			 ON CONFLICT (workspace_id, user_id) DO NOTHING`,
			workspaceID, userID, role,
		); err != nil {
			return fmt.Errorf("add workspace member: %w", err)
		}

		// Join #general if it exists (INSERT ... SELECT yields zero rows if absent).
		if _, err := tx.Exec(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role)
			 SELECT id, $2, 'member' FROM channels WHERE workspace_id = $1 AND slug = 'general' LIMIT 1
			 ON CONFLICT DO NOTHING`,
			workspaceID, userID,
		); err != nil {
			return fmt.Errorf("join general channel: %w", err)
		}

		// Consume the invite; the status guard prevents a concurrent double-accept.
		tag, err := tx.Exec(ctx,
			`UPDATE invitations SET status = 'accepted' WHERE id = $1 AND status = 'pending'`, inviteID)
		if err != nil {
			return fmt.Errorf("consume invite: %w", err)
		}
		if tag.RowsAffected() == 0 {
			return ErrInviteInvalid
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	s.record(ctx, audit.Entry{
		WorkspaceID:  workspaceID,
		ActorID:      userID,
		Action:       audit.ActionInviteAccepted,
		ResourceType: "invitation",
		ResourceID:   inviteID,
		IPAddress:    extractIP(in.IPAddress),
		Metadata:     map[string]interface{}{"email": email, "role": role, "new_account": existing == nil},
	})

	return s.issueTokens(ctx, userID, in.UserAgent, in.IPAddress, MethodPassword)
}

// authorizeInvitee proves that the caller accepting an invite addressed to an
// existing account actually controls that account.
func (s *Service) authorizeInvitee(u *user.User, in AcceptInviteInput) error {
	if !u.IsActive {
		return ErrInvalidCredentials
	}
	var wrongAccount bool
	if in.BearerToken != "" {
		if claims, err := s.jwtMgr.Validate(in.BearerToken); err == nil {
			if claims.UserID == u.ID {
				return nil
			}
			wrongAccount = true
		}
	}
	if in.Password != "" && u.PasswordHash != "" && crypto.CheckPassword(in.Password, u.PasswordHash) {
		return nil
	}
	if wrongAccount {
		return ErrInviteWrongAccount
	}
	return ErrReauthRequired
}

// ChangePassword updates a user's password after verifying the current one,
// then revokes all of the user's sessions so other devices must re-authenticate.
func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword, ipAddress string) error {
	if err := validatePassword(newPassword); err != nil {
		return err
	}

	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return fmt.Errorf("load user: %w", err)
	}
	if u == nil {
		return ErrInvalidCredentials
	}
	// Users created without a password (none currently) may set one freely.
	if u.PasswordHash != "" && !crypto.CheckPassword(oldPassword, u.PasswordHash) {
		return ErrCurrentPassword
	}

	hash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2 WHERE id = $1`, userID, hash); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	if err := s.repo.DeleteUserSessions(ctx, userID); err != nil {
		return err
	}

	s.record(ctx, audit.Entry{
		ActorID:      userID,
		Action:       audit.ActionPasswordChanged,
		ResourceType: "user",
		ResourceID:   userID,
		IPAddress:    extractIP(ipAddress),
	})
	return nil
}

type InviteInfo struct {
	Email         string `json:"email"`
	WorkspaceName string `json:"workspace_name"`
	Role          string `json:"role"`
	InviterName   string `json:"inviter_name"`
}

func (s *Service) GetInviteInfo(ctx context.Context, token string) (*InviteInfo, error) {
	var info InviteInfo
	var status string
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT i.email, w.name, i.role, COALESCE(u.full_name, u.username), i.status, i.expires_at
		 FROM invitations i
		 JOIN workspaces w ON i.workspace_id = w.id
		 JOIN users u ON i.invited_by = u.id
		 WHERE i.token_hash = $1`,
		crypto.HashToken(token),
	).Scan(&info.Email, &info.WorkspaceName, &info.Role, &info.InviterName, &status, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, ErrInviteInvalid
	}
	if err != nil {
		return nil, fmt.Errorf("load invitation: %w", err)
	}
	if status != "pending" {
		return nil, ErrInviteInvalid
	}
	if time.Now().After(expiresAt) {
		return nil, ErrInviteExpired
	}
	return &info, nil
}

func (s *Service) issueTokens(ctx context.Context, userID, userAgent, ipAddress, method string) (*TokenPair, error) {
	accessToken, err := s.jwtMgr.Generate(userID)
	if err != nil {
		return nil, err
	}

	refreshToken, err := crypto.GenerateRandomToken(32)
	if err != nil {
		return nil, err
	}

	session := &Session{
		ID:     uuid.NewString(),
		UserID: userID,
		// Only the hash is persisted; the plaintext leaves in the response.
		RefreshTokenHash: crypto.HashToken(refreshToken),
		UserAgent:        userAgent,
		IPAddress:        extractIP(ipAddress),
		AuthMethod:       method,
		ExpiresAt:        time.Now().Add(s.refreshTTL),
	}

	if err := s.repo.CreateSession(ctx, session); err != nil {
		return nil, err
	}

	return &TokenPair{
		AccessToken:  accessToken,
		RefreshToken: refreshToken,
		ExpiresIn:    int(s.jwtMgr.accessTTL.Seconds()),
	}, nil
}
