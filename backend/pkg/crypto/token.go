package crypto

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"fmt"
)

// GenerateRandomToken returns base64url of `length` bytes from crypto/rand.
// Callers that persist the result MUST store HashToken(tok), never the token.
func GenerateRandomToken(length int) (string, error) {
	b := make([]byte, length)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate token: %w", err)
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}

// HashToken is the at-rest representation of a bearer token (refresh tokens,
// invitation tokens, webhook tokens).
//
// SHA-256 rather than bcrypt: these are >=192 bits of crypto/rand, so there is
// no low-entropy guess space for key stretching to defend, and the value must
// stay cheap enough to serve as an indexed equality lookup. Passwords and TOTP
// backup codes are different — they keep bcrypt (see HashPassword).
func HashToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}

// SecureCompare reports whether two secrets are equal in constant time.
func SecureCompare(a, b string) bool {
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
