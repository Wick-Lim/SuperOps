package sso

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"

	cryptopkg "github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// normalizeEmail lowercases and trims an address from an assertion.
//
// Everything downstream compares addresses, and a provider that emits
// "User@Corp.com" on Monday and "user@corp.com" on Tuesday must not produce two
// accounts. The local part is technically case-sensitive per RFC 5321; no
// deployment on earth treats it that way, and the alternative here is a
// duplicate-account bug that looks exactly like an account takeover.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

func localPart(email string) string {
	if at := strings.IndexByte(email, '@'); at > 0 {
		return email[:at]
	}
	return email
}

// sanitizeUsername reduces an arbitrary claim to something the users.username
// column and the client can both live with.
func sanitizeUsername(raw string) string {
	raw = strings.ToLower(strings.TrimSpace(raw))
	var b strings.Builder
	for _, r := range raw {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
		case r == '.' || r == '_' || r == '-':
			b.WriteRune(r)
		}
		if b.Len() >= 32 {
			break
		}
	}
	out := strings.Trim(b.String(), "._-")
	if len(out) < 2 {
		return ""
	}
	return out
}

// randomSuffix is the disambiguator appended to a colliding username.
func randomSuffix() (string, error) {
	b := make([]byte, 4)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generate username suffix: %w", err)
	}
	return fmt.Sprintf("%x", b), nil
}

// pkce is one Proof Key for Code Exchange pair (RFC 7636).
//
// Mandatory here rather than optional: without it, an authorization code
// intercepted at the redirect — a mis-registered redirect URI, a leaky mobile
// custom-scheme handler, a proxy log — is redeemable by whoever holds it. With
// it, the code is worthless without the verifier, which never leaves this
// process.
type pkce struct {
	Verifier  string
	Challenge string
}

func newPKCE() (pkce, error) {
	// 32 bytes -> 43 base64url characters, the length RFC 7636 §4.1 recommends
	// and the minimum the migration's CHECK constraint accepts.
	verifier, err := cryptopkg.GenerateRandomToken(32)
	if err != nil {
		return pkce{}, err
	}
	sum := sha256.Sum256([]byte(verifier))
	return pkce{
		Verifier:  verifier,
		Challenge: base64.RawURLEncoding.EncodeToString(sum[:]),
	}, nil
}
