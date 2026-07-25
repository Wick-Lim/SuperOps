package sso

import (
	"crypto"
	"crypto/ecdh"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/big"
)

// JSON Web Key parsing, RFC 7517 §4 and RFC 7518 §6, restricted to the two key
// types an OIDC provider actually signs ID tokens with. Anything else (oct in
// particular) is refused rather than skipped silently, so a provider whose key
// set we cannot use produces an error naming the reason.

type jwkDocument struct {
	Keys []jwkKey `json:"keys"`
}

type jwkKey struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Alg string `json:"alg"`
	Use string `json:"use"`

	// RSA
	N string `json:"n"`
	E string `json:"e"`

	// EC
	Crv string `json:"crv"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

// parsedKey is one usable verification key.
type parsedKey struct {
	kid string
	// alg is the algorithm the provider pinned to this key, or "" when it
	// published none. When set, a token header claiming a different algorithm
	// for this kid is rejected.
	alg string
	pub crypto.PublicKey
}

// parseJWKS turns a key set document into the keys we can verify with. Keys we
// cannot use are skipped; an empty result is an error, because "no usable key"
// and "the provider published nothing" fail the same way and both need saying.
func parseJWKS(body []byte) ([]parsedKey, error) {
	var doc jwkDocument
	if err := json.Unmarshal(body, &doc); err != nil {
		return nil, fmt.Errorf("decode jwks: %w", err)
	}

	out := make([]parsedKey, 0, len(doc.Keys))
	for _, k := range doc.Keys {
		// "enc" keys are for encryption, not signatures. An absent `use` means
		// the key may be used for either.
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pub, err := k.publicKey()
		if err != nil || pub == nil {
			continue
		}
		out = append(out, parsedKey{kid: k.Kid, alg: k.Alg, pub: pub})
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("jwks contains no usable signature key (saw %d keys)", len(doc.Keys))
	}
	return out, nil
}

func (k jwkKey) publicKey() (crypto.PublicKey, error) {
	switch k.Kty {
	case "RSA":
		return k.rsaKey()
	case "EC":
		return k.ecKey()
	default:
		return nil, fmt.Errorf("unsupported key type %q", k.Kty)
	}
}

func (k jwkKey) rsaKey() (*rsa.PublicKey, error) {
	n, err := b64uint(k.N)
	if err != nil {
		return nil, fmt.Errorf("rsa modulus: %w", err)
	}
	e, err := b64uint(k.E)
	if err != nil {
		return nil, fmt.Errorf("rsa exponent: %w", err)
	}
	// A modulus below 2048 bits is not a key we should accept a signature from
	// in 2026, and an exponent that does not fit in an int is malformed.
	if n.BitLen() < 2048 {
		return nil, fmt.Errorf("rsa modulus is %d bits, want at least 2048", n.BitLen())
	}
	if !e.IsInt64() || e.Int64() < 3 || e.Int64() > 1<<31-1 {
		return nil, fmt.Errorf("rsa exponent %s is out of range", e)
	}
	return &rsa.PublicKey{N: n, E: int(e.Int64())}, nil
}

func (k jwkKey) ecKey() (*ecdsa.PublicKey, error) {
	var curve elliptic.Curve
	var ecdhCurve ecdh.Curve
	switch k.Crv {
	case "P-256":
		curve, ecdhCurve = elliptic.P256(), ecdh.P256()
	case "P-384":
		curve, ecdhCurve = elliptic.P384(), ecdh.P384()
	case "P-521":
		curve, ecdhCurve = elliptic.P521(), ecdh.P521()
	default:
		return nil, fmt.Errorf("unsupported curve %q", k.Crv)
	}

	x, err := b64uint(k.X)
	if err != nil {
		return nil, fmt.Errorf("ec x: %w", err)
	}
	y, err := b64uint(k.Y)
	if err != nil {
		return nil, fmt.Errorf("ec y: %w", err)
	}

	// A point off the curve is not a key, and verifying a signature against one
	// is undefined behaviour rather than a failed verification. The check goes
	// through crypto/ecdh because elliptic.IsOnCurve is deprecated; encoding the
	// point in SEC 1 uncompressed form and asking NewPublicKey to parse it is
	// the supported way to ask "is this a valid point".
	size := (curve.Params().BitSize + 7) / 8
	if len(x.Bytes()) > size || len(y.Bytes()) > size {
		return nil, fmt.Errorf("ec coordinates are too large for curve %s", k.Crv)
	}
	point := make([]byte, 1+2*size)
	point[0] = 4 // uncompressed
	x.FillBytes(point[1 : 1+size])
	y.FillBytes(point[1+size:])
	if _, err := ecdhCurve.NewPublicKey(point); err != nil {
		return nil, fmt.Errorf("ec point is not on curve %s: %w", k.Crv, err)
	}

	return &ecdsa.PublicKey{Curve: curve, X: x, Y: y}, nil
}

// b64uint decodes a base64url big-endian unsigned integer (RFC 7518 §2).
func b64uint(s string) (*big.Int, error) {
	if s == "" {
		return nil, fmt.Errorf("empty value")
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return nil, fmt.Errorf("decode base64url: %w", err)
	}
	return new(big.Int).SetBytes(b), nil
}
