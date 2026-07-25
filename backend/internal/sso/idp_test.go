package sso

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"
)

// fakeIDP is a minimal but real OpenID Connect provider on httptest.
//
// It exists because the interesting failures are cryptographic: a token signed
// by the wrong key, a token for another audience, a replayed nonce. A stub that
// returns canned claims cannot produce any of those — it can only produce a
// string the code under test was told to reject. Holding the signing key here
// means every negative case is a genuinely invalid token that the verifier has
// to catch on its own.
type fakeIDP struct {
	t      *testing.T
	server *httptest.Server

	keys      map[string]*rsa.PrivateKey
	activeKID string

	clientID     string
	clientSecret string

	mu          sync.Mutex
	codes       map[string]*issuedCode
	jwksFetches int
	// publishedKIDs limits what the JWKS endpoint serves. Empty means "every
	// key"; the rotation test uses it to publish one key at a time.
	publishedKIDs []string
}

type issuedCode struct {
	nonce     string
	challenge string
	spec      issueSpec
}

// issueSpec is everything a test can bend about one issued token.
type issueSpec struct {
	// Claims are merged over the defaults. A value of omit{} removes the claim.
	Claims map[string]any
	// SignKID / SignKey override the header kid and the signing key, which is
	// how a token with a valid structure and an invalid signature is produced.
	SignKID string
	SignKey *rsa.PrivateKey
	// Corrupt flips a bit of the signature after signing.
	Corrupt bool
}

// omit marks a claim that must not appear in the token at all — distinct from
// a claim present and false, which is the difference between "this provider
// does not speak email_verified" and "this address is not verified".
type omit struct{}

func newFakeIDP(t *testing.T) *fakeIDP {
	t.Helper()

	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate idp key: %v", err)
	}

	idp := &fakeIDP{
		t:            t,
		keys:         map[string]*rsa.PrivateKey{"key-1": key},
		activeKID:    "key-1",
		clientID:     "superops-test-client",
		clientSecret: "test-client-secret",
		codes:        map[string]*issuedCode{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("GET /.well-known/openid-configuration", idp.discovery)
	mux.HandleFunc("GET /jwks", idp.jwks)
	mux.HandleFunc("POST /token", idp.token)
	idp.server = httptest.NewServer(mux)
	t.Cleanup(idp.server.Close)

	return idp
}

func (i *fakeIDP) issuer() string { return i.server.URL }

// addKey installs a second signing key, as a provider does mid-rotation.
func (i *fakeIDP) addKey(kid string) *rsa.PrivateKey {
	i.t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		i.t.Fatalf("generate idp key: %v", err)
	}
	i.mu.Lock()
	i.keys[kid] = key
	i.mu.Unlock()
	return key
}

func (i *fakeIDP) publishOnly(kids ...string) {
	i.mu.Lock()
	i.publishedKIDs = kids
	i.mu.Unlock()
}

func (i *fakeIDP) fetchCount() int {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.jwksFetches
}

func (i *fakeIDP) discovery(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, map[string]any{
		"issuer":                                i.server.URL,
		"authorization_endpoint":                i.server.URL + "/authorize",
		"token_endpoint":                        i.server.URL + "/token",
		"jwks_uri":                              i.server.URL + "/jwks",
		"response_types_supported":              []string{"code"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"client_secret_basic", "client_secret_post"},
		"scopes_supported":                      []string{"openid", "email", "profile"},
	})
}

func (i *fakeIDP) jwks(w http.ResponseWriter, _ *http.Request) {
	i.mu.Lock()
	i.jwksFetches++
	published := i.publishedKIDs
	keys := make([]map[string]any, 0, len(i.keys))
	for kid, key := range i.keys {
		if len(published) > 0 && !contains(published, kid) {
			continue
		}
		keys = append(keys, map[string]any{
			"kty": "RSA",
			"kid": kid,
			"alg": "RS256",
			"use": "sig",
			"n":   base64.RawURLEncoding.EncodeToString(key.N.Bytes()),
			"e":   base64.RawURLEncoding.EncodeToString(big.NewInt(int64(key.E)).Bytes()),
		})
	}
	i.mu.Unlock()

	writeJSON(w, map[string]any{"keys": keys})
}

// authorize is the step a browser would perform at the provider. It records
// what the authorization request asked for and returns the code.
func (i *fakeIDP) authorize(nonce, challenge string, spec issueSpec) string {
	i.mu.Lock()
	defer i.mu.Unlock()
	code := "code-" + randomHex()
	i.codes[code] = &issuedCode{nonce: nonce, challenge: challenge, spec: spec}
	return code
}

func (i *fakeIDP) token(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request")
		return
	}

	clientID, clientSecret, ok := r.BasicAuth()
	if !ok {
		clientID, clientSecret = r.Form.Get("client_id"), r.Form.Get("client_secret")
	} else {
		// RFC 6749 §2.3.1 form-urlencodes both halves before base64.
		clientID, clientSecret = mustUnescape(i.t, clientID), mustUnescape(i.t, clientSecret)
	}
	if clientID != i.clientID || clientSecret != i.clientSecret {
		oauthError(w, http.StatusUnauthorized, "invalid_client")
		return
	}

	i.mu.Lock()
	rec, found := i.codes[r.Form.Get("code")]
	delete(i.codes, r.Form.Get("code")) // authorization codes are single use
	i.mu.Unlock()
	if !found {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}

	// PKCE: the verifier must hash to the challenge sent at authorization time.
	sum := sha256.Sum256([]byte(r.Form.Get("code_verifier")))
	if base64.RawURLEncoding.EncodeToString(sum[:]) != rec.challenge {
		oauthError(w, http.StatusBadRequest, "invalid_grant")
		return
	}

	writeJSON(w, map[string]any{
		"access_token": "access-" + randomHex(),
		"id_token":     i.signIDToken(rec),
		"token_type":   "Bearer",
		"expires_in":   3600,
	})
}

func (i *fakeIDP) signIDToken(rec *issuedCode) string {
	now := time.Now()
	claims := map[string]any{
		"iss":            i.server.URL,
		"sub":            "idp-subject-1",
		"aud":            i.clientID,
		"exp":            now.Add(5 * time.Minute).Unix(),
		"iat":            now.Unix(),
		"nonce":          rec.nonce,
		"email":          "person@example.com",
		"email_verified": true,
		"name":           "Test Person",
	}
	for k, v := range rec.spec.Claims {
		if _, drop := v.(omit); drop {
			delete(claims, k)
			continue
		}
		claims[k] = v
	}

	kid := rec.spec.SignKID
	if kid == "" {
		kid = i.activeKID
	}
	key := rec.spec.SignKey
	if key == nil {
		i.mu.Lock()
		key = i.keys[kid]
		i.mu.Unlock()
	}
	if key == nil {
		i.t.Fatalf("no signing key for kid %q", kid)
	}

	return signJWT(i.t, key, kid, claims, rec.spec.Corrupt)
}

func signJWT(t *testing.T, key *rsa.PrivateKey, kid string, claims map[string]any, corrupt bool) string {
	t.Helper()

	header, err := json.Marshal(map[string]any{"alg": "RS256", "kid": kid, "typ": "JWT"})
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatalf("marshal claims: %v", err)
	}

	signingInput := base64.RawURLEncoding.EncodeToString(header) + "." + base64.RawURLEncoding.EncodeToString(payload)
	sum := sha256.Sum256([]byte(signingInput))
	sig, err := rsa.SignPKCS1v15(rand.Reader, key, crypto.SHA256, sum[:])
	if err != nil {
		t.Fatalf("sign: %v", err)
	}
	if corrupt {
		sig[0] ^= 0xff
	}
	return signingInput + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func writeJSON(w http.ResponseWriter, body any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(body)
}

func oauthError(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": code})
}

func randomHex() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}

func mustUnescape(t *testing.T, s string) string {
	t.Helper()
	out, err := url.QueryUnescape(s)
	if err != nil {
		t.Fatalf("unescape %q: %v", s, err)
	}
	return out
}
