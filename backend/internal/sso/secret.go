package sso

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
)

// aesKeyBytes is the AES-256 key length. Nothing shorter is accepted: a
// deployment that can supply 16 bytes can supply 32.
const aesKeyBytes = 32

// ParseSecretKey reads SSO_SECRET_KEY in whichever of the three shapes an
// operator is likely to produce it — `openssl rand -hex 32`,
// `openssl rand -base64 32`, or 32 raw characters — and rejects everything
// else. Guessing wrong here is silent: a key that decodes to the wrong bytes
// still "works" until the next process starts with a different guess and
// cannot open the secrets the previous one sealed.
func ParseSecretKey(raw string) ([]byte, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil, nil
	}

	if len(raw) == hex.EncodedLen(aesKeyBytes) {
		if b, err := hex.DecodeString(raw); err == nil {
			return b, nil
		}
	}
	// Both padded ("...=") and raw base64 of 32 bytes.
	for _, enc := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if b, err := enc.DecodeString(raw); err == nil && len(b) == aesKeyBytes {
			return b, nil
		}
	}
	if len(raw) == aesKeyBytes {
		return []byte(raw), nil
	}

	return nil, fmt.Errorf("SSO_SECRET_KEY must be %d bytes, given as %d hex characters, base64, or %d raw characters (got %d characters)",
		aesKeyBytes, hex.EncodedLen(aesKeyBytes), aesKeyBytes, len(raw))
}

// sealSecret encrypts an OIDC client secret for storage.
//
// AES-256-GCM with a fresh random nonce prefixed to the ciphertext. The client
// secret is a symmetric credential we have to replay to the token endpoint, so
// unlike a session token it cannot be hashed — encryption is the only option,
// and leaving it in plaintext would mean one SELECT on sso_providers hands over
// every tenant's IdP credentials.
func sealSecret(key []byte, plaintext string) ([]byte, error) {
	if len(key) != aesKeyBytes {
		return nil, ErrNotConfigured
	}
	if plaintext == "" {
		return nil, nil
	}

	gcm, err := newGCM(key)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, fmt.Errorf("generate nonce: %w", err)
	}
	return gcm.Seal(nonce, nonce, []byte(plaintext), nil), nil
}

// openSecret reverses sealSecret. A failure here is an internal fault (wrong
// key, corrupted row), never a caller fault.
func openSecret(key []byte, sealed []byte) (string, error) {
	if len(sealed) == 0 {
		return "", nil
	}
	if len(key) != aesKeyBytes {
		return "", ErrNotConfigured
	}

	gcm, err := newGCM(key)
	if err != nil {
		return "", err
	}
	if len(sealed) < gcm.NonceSize() {
		return "", errors.New("open client secret: ciphertext is shorter than the nonce")
	}
	plaintext, err := gcm.Open(nil, sealed[:gcm.NonceSize()], sealed[gcm.NonceSize():], nil)
	if err != nil {
		// Almost always SSO_SECRET_KEY having changed. Say so, because the
		// alternative reading ("the provider is broken") sends an operator to
		// the wrong place entirely.
		return "", fmt.Errorf("open client secret (has SSO_SECRET_KEY changed?): %w", err)
	}
	return string(plaintext), nil
}

func newGCM(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("build cipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("build gcm: %w", err)
	}
	return gcm, nil
}
