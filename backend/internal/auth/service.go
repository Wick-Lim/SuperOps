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

	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

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
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func NewService(repo *Repository, userRepo *user.Repository, pool *pgxpool.Pool, jwtMgr *JWTManager, refreshTTL time.Duration) *Service {
	return &Service{
		repo:       repo,
		userRepo:   userRepo,
		pool:       pool,
		jwtMgr:     jwtMgr,
		refreshTTL: refreshTTL,
	}
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
	TOTPCode string `json:"totp_code"`
}

func (s *Service) Login(ctx context.Context, input LoginInput, userAgent, ipAddress string) (*TokenPair, error) {
	u, err := s.userRepo.GetByEmail(ctx, input.Email)
	if err != nil {
		return nil, err
	}
	if u == nil {
		return nil, fmt.Errorf("invalid credentials")
	}
	if !u.IsActive {
		return nil, fmt.Errorf("account is deactivated")
	}

	if !crypto.CheckPassword(input.Password, u.PasswordHash) {
		return nil, fmt.Errorf("invalid credentials")
	}

	// Second factor, if enabled.
	var secret string
	var totpEnabled bool
	s.pool.QueryRow(ctx, `SELECT COALESCE(totp_secret,''), totp_enabled FROM users WHERE id = $1`, u.ID).Scan(&secret, &totpEnabled)
	if totpEnabled {
		if input.TOTPCode == "" {
			return nil, ErrTOTPRequired
		}
		if !s.verifyTOTPOrBackup(ctx, u.ID, secret, input.TOTPCode) {
			return nil, fmt.Errorf("invalid 2fa code")
		}
	}

	return s.issueTokens(ctx, u.ID, userAgent, ipAddress)
}

func (s *Service) RefreshTokens(ctx context.Context, refreshToken, userAgent, ipAddress string) (*TokenPair, error) {
	session, err := s.repo.GetSessionByToken(ctx, refreshToken)
	if err != nil {
		return nil, err
	}
	if session == nil {
		return nil, fmt.Errorf("invalid refresh token")
	}
	if time.Now().After(session.ExpiresAt) {
		s.repo.DeleteSession(ctx, session.ID)
		return nil, fmt.Errorf("refresh token expired")
	}

	if err := s.repo.DeleteSession(ctx, session.ID); err != nil {
		return nil, err
	}

	return s.issueTokens(ctx, session.UserID, userAgent, ipAddress)
}

func (s *Service) Logout(ctx context.Context, refreshToken string) error {
	session, err := s.repo.GetSessionByToken(ctx, refreshToken)
	if err != nil {
		return err
	}
	if session == nil {
		return nil
	}
	return s.repo.DeleteSession(ctx, session.ID)
}

// AcceptInvite validates an invite token, creates a user, adds to workspace, and returns tokens.
func (s *Service) AcceptInvite(ctx context.Context, token, username, password, fullName, userAgent, ipAddress string) (*TokenPair, error) {
	// Validate invite token
	var inviteID, email, workspaceID, role, status string
	var expiresAt time.Time
	err := s.pool.QueryRow(ctx,
		`SELECT id, email, workspace_id, role, status, expires_at FROM invitations WHERE token = $1`,
		token,
	).Scan(&inviteID, &email, &workspaceID, &role, &status, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("invalid invite token")
	}
	if status != "pending" {
		return nil, fmt.Errorf("invite already used")
	}
	if time.Now().After(expiresAt) {
		s.pool.Exec(ctx, `UPDATE invitations SET status = 'expired' WHERE id = $1`, inviteID)
		return nil, fmt.Errorf("invite expired")
	}

	// Check username availability
	existing, _ := s.userRepo.GetByUsername(ctx, username)
	if existing != nil {
		return nil, fmt.Errorf("username already taken")
	}

	// Create user
	hash, err := crypto.HashPassword(password)
	if err != nil {
		return nil, err
	}

	userID := uuid.NewString()

	// Create the account, add it to the workspace + #general, and consume the
	// invite atomically: a partial failure must never leave an orphaned user.
	err = database.WithTx(ctx, s.pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx,
			`INSERT INTO users (id, email, username, full_name, password_hash, is_active)
			 VALUES ($1, $2, $3, $4, $5, true)`,
			userID, email, username, fullName, hash,
		); err != nil {
			return fmt.Errorf("create user: %w", err)
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
			return errors.New("invite already used")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}

	return s.issueTokens(ctx, userID, userAgent, ipAddress)
}

// ChangePassword updates a user's password after verifying the current one,
// then revokes all of the user's sessions so other devices must re-authenticate.
func (s *Service) ChangePassword(ctx context.Context, userID, oldPassword, newPassword string) error {
	u, err := s.userRepo.GetByID(ctx, userID)
	if err != nil {
		return err
	}
	if u == nil {
		return errors.New("user not found")
	}
	// Users created without a password (none currently) may set one freely.
	if u.PasswordHash != "" && !crypto.CheckPassword(oldPassword, u.PasswordHash) {
		return errors.New("current password is incorrect")
	}

	hash, err := crypto.HashPassword(newPassword)
	if err != nil {
		return err
	}
	if _, err := s.pool.Exec(ctx,
		`UPDATE users SET password_hash = $2, updated_at = NOW() WHERE id = $1`, userID, hash); err != nil {
		return fmt.Errorf("update password: %w", err)
	}

	_ = s.repo.DeleteUserSessions(ctx, userID)
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
		 WHERE i.token = $1`,
		token,
	).Scan(&info.Email, &info.WorkspaceName, &info.Role, &info.InviterName, &status, &expiresAt)
	if err != nil {
		return nil, fmt.Errorf("invite not found")
	}
	if status != "pending" {
		return nil, fmt.Errorf("invite already used")
	}
	if time.Now().After(expiresAt) {
		return nil, fmt.Errorf("invite expired")
	}
	return &info, nil
}

func (s *Service) issueTokens(ctx context.Context, userID, userAgent, ipAddress string) (*TokenPair, error) {
	accessToken, err := s.jwtMgr.Generate(userID)
	if err != nil {
		return nil, err
	}

	refreshToken, err := crypto.GenerateRandomToken(32)
	if err != nil {
		return nil, err
	}

	session := &Session{
		ID:           uuid.NewString(),
		UserID:       userID,
		RefreshToken: refreshToken,
		UserAgent:    userAgent,
		IPAddress:    extractIP(ipAddress),
		ExpiresAt:    time.Now().Add(s.refreshTTL),
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
