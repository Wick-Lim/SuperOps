package auth

import (
	"testing"
	"time"
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

func TestJWTWrongSecret(t *testing.T) {
	mgr1 := NewJWTManager("secret-one-32-chars-long-enough", 15*time.Minute)
	mgr2 := NewJWTManager("secret-two-32-chars-long-enough", 15*time.Minute)

	token, _ := mgr1.Generate("user-1")
	_, err := mgr2.Validate(token)
	if err == nil {
		t.Fatal("expected error for wrong secret")
	}
}
