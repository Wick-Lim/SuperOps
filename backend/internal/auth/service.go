package auth

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
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
	u := &user.User{
		ID:           userID,
		Email:        email,
		Username:     username,
		FullName:     fullName,
		PasswordHash: hash,
		IsActive:     true,
	}
	if err := s.userRepo.Create(ctx, u); err != nil {
		return nil, fmt.Errorf("create user: %w", err)
	}

	// Add to workspace
	s.pool.Exec(ctx,
		`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, $3)`,
		workspaceID, userID, role,
	)

	// Add to #general channel
	s.pool.Exec(ctx,
		`INSERT INTO channel_members (channel_id, user_id, role) VALUES (
			(SELECT id FROM channels WHERE workspace_id = $1 AND slug = 'general' LIMIT 1), $2, 'member'
		)`, workspaceID, userID,
	)

	// Mark invite as accepted
	s.pool.Exec(ctx, `UPDATE invitations SET status = 'accepted' WHERE id = $1`, inviteID)

	return s.issueTokens(ctx, userID, userAgent, ipAddress)
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
