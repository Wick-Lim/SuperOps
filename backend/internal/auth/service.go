package auth

import (
	"context"
	"fmt"
	"net"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/user"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

func extractIP(remoteAddr string) string {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return remoteAddr
	}
	// Convert IPv6 loopback to IPv4 for PostgreSQL INET compatibility
	if host == "::1" {
		return "127.0.0.1"
	}
	return host
}

type Service struct {
	repo        *Repository
	userRepo    *user.Repository
	jwtMgr      *JWTManager
	refreshTTL  time.Duration
}

type TokenPair struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	ExpiresIn    int    `json:"expires_in"`
}

func NewService(repo *Repository, userRepo *user.Repository, jwtMgr *JWTManager, refreshTTL time.Duration) *Service {
	return &Service{
		repo:       repo,
		userRepo:   userRepo,
		jwtMgr:     jwtMgr,
		refreshTTL: refreshTTL,
	}
}

type RegisterInput struct {
	Email    string `json:"email"`
	Username string `json:"username"`
	Password string `json:"password"`
	FullName string `json:"full_name"`
}

type LoginInput struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

func (s *Service) Register(ctx context.Context, input RegisterInput) (*user.User, error) {
	existing, err := s.userRepo.GetByEmail(ctx, input.Email)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("email already registered")
	}

	existing, err = s.userRepo.GetByUsername(ctx, input.Username)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		return nil, fmt.Errorf("username already taken")
	}

	hash, err := crypto.HashPassword(input.Password)
	if err != nil {
		return nil, err
	}

	u := &user.User{
		ID:           uuid.NewString(),
		Email:        input.Email,
		Username:     input.Username,
		FullName:     input.FullName,
		PasswordHash: hash,
		IsActive:     true,
	}

	if err := s.userRepo.Create(ctx, u); err != nil {
		return nil, err
	}

	return u, nil
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

	// Rotate: delete old session, issue new tokens
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
