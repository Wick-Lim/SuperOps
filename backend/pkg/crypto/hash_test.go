package crypto

import "testing"

func TestHashAndCheckPassword(t *testing.T) {
	password := "MySecureP@ss123"
	hash, err := HashPassword(password)
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	if hash == "" {
		t.Fatal("hash is empty")
	}
	if hash == password {
		t.Fatal("hash should not equal password")
	}

	if !CheckPassword(password, hash) {
		t.Error("correct password should match")
	}
	if CheckPassword("wrong-password", hash) {
		t.Error("wrong password should not match")
	}
}

func TestGenerateRandomToken(t *testing.T) {
	t1, err := GenerateRandomToken(32)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	t2, _ := GenerateRandomToken(32)

	if t1 == "" || t2 == "" {
		t.Fatal("tokens should not be empty")
	}
	if t1 == t2 {
		t.Fatal("tokens should be unique")
	}
}
