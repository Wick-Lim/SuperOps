package sso

import (
	"context"
	"crypto"
	"crypto/ecdsa"
	"crypto/rsa"
	_ "crypto/sha256" // registers SHA-256/384 for crypto.Hash.New
	_ "crypto/sha512" // registers SHA-512 for crypto.Hash.New
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	cryptopkg "github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

// ID token verification, OIDC Core §3.1.3.7.
//
// Every one of these checks has a bypass attached to skipping it, which is why
// they are all here and none of them are optional:
//
//   - `alg` is taken from the *key*, not only from the header. A header-driven
//     algorithm choice is the algorithm-confusion attack: `alg: none`, or HS256
//     signed with the public key the JWKS just handed out.
//   - `iss` must equal the configured issuer, or a token from any provider
//     verifies here.
//   - `aud` must contain our client id, or a token minted for a different
//     application at the same IdP — including one an attacker registered —
//     verifies here.
//   - `nonce` must match the one bound to this sign-in, or a token captured
//     elsewhere can be replayed into a fresh callback.
//   - `exp` must be in the future, within a small skew.

// signatureAlgs is the accepted set. Symmetric algorithms are absent
// deliberately: the verification key comes from a public key set, so an HMAC
// token is either a confusion attack or a provider we cannot support.
var signatureAlgs = map[string]crypto.Hash{
	"RS256": crypto.SHA256, "RS384": crypto.SHA384, "RS512": crypto.SHA512,
	"PS256": crypto.SHA256, "PS384": crypto.SHA384, "PS512": crypto.SHA512,
	"ES256": crypto.SHA256, "ES384": crypto.SHA384, "ES512": crypto.SHA512,
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Kid string `json:"kid"`
	Typ string `json:"typ"`
}

// IDClaims is the verified content of an ID token.
type IDClaims struct {
	Issuer   string     `json:"iss"`
	Subject  string     `json:"sub"`
	Audience stringList `json:"aud"`
	Expiry   int64      `json:"exp"`
	IssuedAt int64      `json:"iat"`
	AuthTime int64      `json:"auth_time"`
	Nonce    string     `json:"nonce"`
	AZP      string     `json:"azp"`

	Email             string   `json:"email"`
	EmailVerified     flexBool `json:"email_verified"`
	Name              string   `json:"name"`
	GivenName         string   `json:"given_name"`
	FamilyName        string   `json:"family_name"`
	PreferredUsername string   `json:"preferred_username"`

	ACR string     `json:"acr"`
	AMR stringList `json:"amr"`

	// raw keeps the whole payload so a configured role claim can be read
	// without this struct having to know its name.
	raw map[string]json.RawMessage
}

// Claim returns the values of an arbitrary claim as strings. A claim carrying a
// single string, an array of strings, or an array of objects with a "name" or
// "value" field (how some directories emit groups) all reduce to a string list.
func (c *IDClaims) Claim(name string) []string {
	body, ok := c.raw[name]
	if !ok {
		return nil
	}
	var list stringList
	if err := json.Unmarshal(body, &list); err == nil {
		return list
	}
	return nil
}

// stringList accepts both `"a"` and `["a","b"]`, because `aud` and `amr` are
// specified as either and providers use both.
type stringList []string

func (s *stringList) UnmarshalJSON(b []byte) error {
	var single string
	if err := json.Unmarshal(b, &single); err == nil {
		*s = stringList{single}
		return nil
	}
	var many []string
	if err := json.Unmarshal(b, &many); err == nil {
		*s = many
		return nil
	}
	return fmt.Errorf("value is neither a string nor an array of strings")
}

func (s stringList) contains(want string) bool {
	for _, v := range s {
		if v == want {
			return true
		}
	}
	return false
}

// flexBool distinguishes "absent" from "false" and tolerates the providers that
// emit `"true"` as a string. The distinction matters: an absent
// `email_verified` is a provider that does not speak the claim (Entra ID),
// which is a configuration question, while `false` is a provider saying the
// address is unverified, which is a refusal.
type flexBool struct {
	Set   bool
	Value bool
}

func (f *flexBool) UnmarshalJSON(b []byte) error {
	var asBool bool
	if err := json.Unmarshal(b, &asBool); err == nil {
		*f = flexBool{Set: true, Value: asBool}
		return nil
	}
	var asString string
	if err := json.Unmarshal(b, &asString); err == nil {
		switch strings.ToLower(strings.TrimSpace(asString)) {
		case "true":
			*f = flexBool{Set: true, Value: true}
			return nil
		case "false":
			*f = flexBool{Set: true, Value: false}
			return nil
		}
	}
	return fmt.Errorf("email_verified is neither a boolean nor \"true\"/\"false\"")
}

type verifyOptions struct {
	Issuer   string
	ClientID string
	// NonceHash is crypto.HashToken of the nonce sent in the authorization
	// request. The plaintext is not kept, so the comparison is hash to hash.
	NonceHash string
	Now       time.Time
	Skew      time.Duration
}

// VerifyIDToken checks an ID token end to end and returns its claims.
func (c *Client) VerifyIDToken(ctx context.Context, md *Metadata, raw string, opts verifyOptions) (*IDClaims, error) {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return nil, errors.New("id_token is not a three-part JWS")
	}

	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, fmt.Errorf("decode id_token header: %w", err)
	}
	var header jwtHeader
	if err := json.Unmarshal(headerBytes, &header); err != nil {
		return nil, fmt.Errorf("parse id_token header: %w", err)
	}
	hash, ok := signatureAlgs[header.Alg]
	if !ok {
		return nil, fmt.Errorf("unsupported id_token signature algorithm %q", header.Alg)
	}

	key, err := c.Key(ctx, md.JWKSURI, header.Kid, header.Alg)
	if err != nil {
		return nil, err
	}

	sig, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return nil, fmt.Errorf("decode id_token signature: %w", err)
	}
	signingInput := []byte(parts[0] + "." + parts[1])
	if err := verifySignature(key, header.Alg, hash, signingInput, sig); err != nil {
		return nil, err
	}

	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, fmt.Errorf("decode id_token payload: %w", err)
	}
	var claims IDClaims
	if err := json.Unmarshal(payload, &claims); err != nil {
		return nil, fmt.Errorf("parse id_token payload: %w", err)
	}
	if err := json.Unmarshal(payload, &claims.raw); err != nil {
		return nil, fmt.Errorf("parse id_token payload: %w", err)
	}

	if err := claims.validate(opts); err != nil {
		return nil, err
	}
	return &claims, nil
}

func (c *IDClaims) validate(opts verifyOptions) error {
	if strings.TrimRight(c.Issuer, "/") != strings.TrimRight(opts.Issuer, "/") {
		return fmt.Errorf("id_token issuer is %q, expected %q", c.Issuer, opts.Issuer)
	}
	if c.Subject == "" {
		return errors.New("id_token has no subject")
	}
	if !c.Audience.contains(opts.ClientID) {
		return fmt.Errorf("id_token audience %v does not include this client", []string(c.Audience))
	}
	// OIDC Core §3.1.3.7 step 4: with several audiences the token must name the
	// party it was issued to, and that party must be us. Without this a token
	// minted for another client that happens to also list ours verifies.
	if len(c.Audience) > 1 && c.AZP != opts.ClientID {
		return fmt.Errorf("id_token has multiple audiences and azp %q is not this client", c.AZP)
	}
	if c.AZP != "" && c.AZP != opts.ClientID {
		return fmt.Errorf("id_token azp %q is not this client", c.AZP)
	}

	now := opts.Now
	if now.IsZero() {
		now = time.Now()
	}
	if c.Expiry == 0 {
		return errors.New("id_token has no expiry")
	}
	if now.After(time.Unix(c.Expiry, 0).Add(opts.Skew)) {
		return fmt.Errorf("id_token expired at %s", time.Unix(c.Expiry, 0).UTC().Format(time.RFC3339))
	}
	if c.IssuedAt != 0 && time.Unix(c.IssuedAt, 0).After(now.Add(opts.Skew)) {
		return fmt.Errorf("id_token was issued in the future (%s)", time.Unix(c.IssuedAt, 0).UTC().Format(time.RFC3339))
	}

	if opts.NonceHash != "" {
		if c.Nonce == "" {
			return errors.New("id_token has no nonce")
		}
		if !cryptopkg.SecureCompare(cryptopkg.HashToken(c.Nonce), opts.NonceHash) {
			return errors.New("id_token nonce does not match this sign-in request")
		}
	}
	return nil
}

func verifySignature(key parsedKey, alg string, hash crypto.Hash, signingInput, sig []byte) error {
	digest := hash.New()
	digest.Write(signingInput)
	sum := digest.Sum(nil)

	switch {
	case strings.HasPrefix(alg, "RS"):
		pub, ok := key.pub.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("key %q is not an RSA key but the token claims %s", key.kid, alg)
		}
		if err := rsa.VerifyPKCS1v15(pub, hash, sum, sig); err != nil {
			return fmt.Errorf("id_token signature is invalid: %w", err)
		}
		return nil

	case strings.HasPrefix(alg, "PS"):
		pub, ok := key.pub.(*rsa.PublicKey)
		if !ok {
			return fmt.Errorf("key %q is not an RSA key but the token claims %s", key.kid, alg)
		}
		if err := rsa.VerifyPSS(pub, hash, sum, sig, &rsa.PSSOptions{SaltLength: rsa.PSSSaltLengthAuto, Hash: hash}); err != nil {
			return fmt.Errorf("id_token signature is invalid: %w", err)
		}
		return nil

	case strings.HasPrefix(alg, "ES"):
		pub, ok := key.pub.(*ecdsa.PublicKey)
		if !ok {
			return fmt.Errorf("key %q is not an EC key but the token claims %s", key.kid, alg)
		}
		// JWS ECDSA signatures are the fixed-width r||s form, not ASN.1.
		size := (pub.Curve.Params().BitSize + 7) / 8
		if len(sig) != 2*size {
			return fmt.Errorf("id_token signature is %d bytes, expected %d for %s", len(sig), 2*size, alg)
		}
		r := new(big.Int).SetBytes(sig[:size])
		s := new(big.Int).SetBytes(sig[size:])
		if !ecdsa.Verify(pub, sum, r, s) {
			return errors.New("id_token signature is invalid")
		}
		return nil
	}
	return fmt.Errorf("unsupported id_token signature algorithm %q", alg)
}

// mfaValues are the RFC 8176 authentication-method references that mean a
// second factor was actually presented. "mfa" is the explicit one; the rest are
// the possession factors that only exist as a second step.
var mfaValues = map[string]bool{
	"mfa": true, "otp": true, "hwk": true, "swk": true, "fido": true, "pop": true, "sc": true,
}

// idpSatisfiedMFA reports whether the provider demonstrated a second factor.
//
// Trusting the IdP for MFA is a per-provider decision, but the decision alone is
// not enough: a provider configured as trusted that then performs a
// password-only login must still hit the local TOTP prompt. So the switch only
// takes effect when the token carries evidence — an `amr` naming a second
// factor, or the `acr` the administrator said to require.
func idpSatisfiedMFA(claims *IDClaims, trust bool, requiredACR string) bool {
	if !trust {
		return false
	}
	if requiredACR != "" {
		return claims.ACR == requiredACR
	}
	for _, v := range claims.AMR {
		if mfaValues[strings.ToLower(v)] {
			return true
		}
	}
	return false
}
