package auth

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

func TestJWTGenerateAndValidate(t *testing.T) {
	mgr := NewJWTManager("test-secret-32-chars-long-enough", 15*time.Minute)

	token, err := mgr.Generate("user-123")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if token == "" {
		t.Fatal("token is empty")
	}

	claims, err := mgr.Validate(token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if claims.UserID != "user-123" {
		t.Errorf("expected user-123, got %s", claims.UserID)
	}
}

func TestJWTGenerateWithWorkspace(t *testing.T) {
	mgr := NewJWTManager("test-secret-32-chars-long-enough", 15*time.Minute)

	token, err := mgr.GenerateWithWorkspace("user-1", "ws-1", "admin")
	if err != nil {
		t.Fatalf("generate: %v", err)
	}

	claims, err := mgr.Validate(token)
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if claims.UserID != "user-1" || claims.WorkspaceID != "ws-1" || claims.Role != "admin" {
		t.Errorf("unexpected claims: %+v", claims)
	}
}

func TestJWTInvalidToken(t *testing.T) {
	mgr := NewJWTManager("test-secret-32-chars-long-enough", 15*time.Minute)
	_, err := mgr.Validate("invalid-token")
	if err == nil {
		t.Fatal("expected error for invalid token")
	}
}

// We stamp iss on every token we mint, so we must reject a correctly signed
// token that carries a different issuer.
func TestJWTWrongIssuer(t *testing.T) {
	const secret = "test-secret-32-chars-long-enough"
	mgr := NewJWTManager(secret, 15*time.Minute)

	now := time.Now()
	foreign := jwt.NewWithClaims(jwt.SigningMethodHS256, Claims{
		UserID: "user-1",
		RegisteredClaims: jwt.RegisteredClaims{
			ExpiresAt: jwt.NewNumericDate(now.Add(15 * time.Minute)),
			IssuedAt:  jwt.NewNumericDate(now),
			Issuer:    "somebody-else",
		},
	})
	signed, err := foreign.SignedString([]byte(secret))
	if err != nil {
		t.Fatalf("sign: %v", err)
	}

	if _, err := mgr.Validate(signed); err == nil {
		t.Fatal("expected a token from another issuer to be rejected")
	}
}

func TestJWTWrongSecret(t *testing.T) {
	mgr1 := NewJWTManager("secret-one-32-chars-long-enough", 15*time.Minute)
	mgr2 := NewJWTManager("secret-two-32-chars-long-enough", 15*time.Minute)

	token, _ := mgr1.Generate("user-1")
	_, err := mgr2.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}
