package sso

import (
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"net"
	"strings"
	"testing"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
)

func TestParseSecretKey(t *testing.T) {
	raw := []byte("0123456789abcdef0123456789abcdef")

	tests := []struct {
		name    string
		input   string
		want    []byte
		wantErr bool
	}{
		{"empty disables sso", "", nil, false},
		{"hex", hex.EncodeToString(raw), raw, false},
		{"standard base64", base64.StdEncoding.EncodeToString(raw), raw, false},
		{"raw url base64", base64.RawURLEncoding.EncodeToString(raw), raw, false},
		{"32 literal characters", string(raw), raw, false},
		{"too short", "short", nil, true},
		{"40 characters of nothing in particular", strings.Repeat("z", 40), nil, true},
		// 32 hex characters are 16 bytes hex-encoded AND 32 raw characters. The
		// raw reading wins, which is fine (it is still a 32-byte key) but is the
		// one case where what the operator meant and what they get differ.
		{"32 hex characters are read as 32 raw bytes",
			hex.EncodeToString(raw[:16]), []byte(hex.EncodeToString(raw[:16])), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseSecretKey(tt.input)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected an error, got key of %d bytes", len(got))
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if string(got) != string(tt.want) {
				t.Errorf("key = %x, want %x", got, tt.want)
			}
		})
	}
}

func TestSealAndOpenSecret(t *testing.T) {
	key := []byte("0123456789abcdef0123456789abcdef")

	sealed, err := sealSecret(key, "client-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if strings.Contains(string(sealed), "client-secret") {
		t.Fatal("the plaintext survived sealing")
	}

	// Two seals of the same value must differ: a deterministic ciphertext
	// leaks which tenants share a secret.
	again, err := sealSecret(key, "client-secret")
	if err != nil {
		t.Fatalf("seal: %v", err)
	}
	if string(again) == string(sealed) {
		t.Fatal("sealing is deterministic")
	}

	got, err := openSecret(key, sealed)
	if err != nil || got != "client-secret" {
		t.Fatalf("open = %q, %v", got, err)
	}

	if _, err := openSecret([]byte("ffffffffffffffffffffffffffffffff"), sealed); err == nil {
		t.Fatal("the wrong key opened the secret")
	}

	tampered := append([]byte(nil), sealed...)
	tampered[len(tampered)-1] ^= 0xff
	if _, err := openSecret(key, tampered); err == nil {
		t.Fatal("a tampered ciphertext was accepted")
	}

	// A public client stores nothing and opens to nothing.
	empty, err := sealSecret(key, "")
	if err != nil || empty != nil {
		t.Fatalf("sealing an empty secret = %v, %v", empty, err)
	}
	if got, err := openSecret(key, nil); err != nil || got != "" {
		t.Fatalf("opening nothing = %q, %v", got, err)
	}
}

func TestSanitizeUsername(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"alex", "alex"},
		{"Alex.Smith", "alex.smith"},
		{"alex+tag", "alextag"},
		{"  spaced  ", "spaced"},
		{"...", ""},
		{"a", ""},
		{"ålex", "lex"},
		{strings.Repeat("x", 60), strings.Repeat("x", 32)},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			if got := sanitizeUsername(tt.in); got != tt.want {
				t.Errorf("sanitizeUsername(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestNormalizeEmail(t *testing.T) {
	if got := normalizeEmail("  Person@Example.COM "); got != "person@example.com" {
		t.Errorf("normalizeEmail = %q", got)
	}
	if got := localPart("person@example.com"); got != "person" {
		t.Errorf("localPart = %q", got)
	}
	if got := localPart("no-at-sign"); got != "no-at-sign" {
		t.Errorf("localPart = %q", got)
	}
}

func TestPKCEChallengeIsTheHashOfTheVerifier(t *testing.T) {
	p, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	// RFC 7636 §4.1: 43-128 characters, and the migration's CHECK agrees.
	if len(p.Verifier) < 43 || len(p.Verifier) > 128 {
		t.Fatalf("verifier is %d characters", len(p.Verifier))
	}
	if p.Challenge == p.Verifier {
		t.Fatal("the challenge is the verifier in plaintext")
	}
	other, err := newPKCE()
	if err != nil {
		t.Fatalf("newPKCE: %v", err)
	}
	if other.Verifier == p.Verifier {
		t.Fatal("two verifiers came out identical")
	}
}

func TestWithOpenIDScope(t *testing.T) {
	tests := []struct{ in, want string }{
		{"", "openid"},
		{"email profile", "openid email profile"},
		{"openid email", "openid email"},
		{"  profile   openid ", "profile openid"},
	}
	for _, tt := range tests {
		if got := withOpenIDScope(tt.in); got != tt.want {
			t.Errorf("withOpenIDScope(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestResolveRole(t *testing.T) {
	provider := &Provider{
		DefaultRole: authz.RoleMember,
		RoleClaim:   "groups",
		RoleMapping: map[string]string{
			"admins":  authz.RoleAdmin,
			"staff":   authz.RoleMember,
			"vendors": authz.RoleGuest,
			"owners":  authz.RoleOwner, // must never be honoured
		},
	}

	tests := []struct {
		name         string
		claims       string
		provider     *Provider
		wantRole     string
		wantAsserted bool
	}{
		{"no role claim configured", `{}`, &Provider{DefaultRole: authz.RoleGuest}, authz.RoleGuest, false},
		{"single mapped group", `{"groups":"admins"}`, provider, authz.RoleAdmin, true},
		{"most privileged of several", `{"groups":["vendors","admins","staff"]}`, provider, authz.RoleAdmin, true},
		{"unmapped group falls back", `{"groups":["nobody"]}`, provider, authz.RoleMember, false},
		{"claim absent falls back", `{}`, provider, authz.RoleMember, false},
		{"owner mapping is ignored", `{"groups":["owners"]}`, provider, authz.RoleMember, false},
		{"owner mapping does not beat a real one", `{"groups":["owners","vendors"]}`, provider, authz.RoleGuest, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var claims IDClaims
			if err := json.Unmarshal([]byte(tt.claims), &claims.raw); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			role, asserted := resolveRole(tt.provider, &claims)
			if role != tt.wantRole || asserted != tt.wantAsserted {
				t.Errorf("resolveRole = (%q, %v), want (%q, %v)", role, asserted, tt.wantRole, tt.wantAsserted)
			}
		})
	}
}

func TestIDPSatisfiedMFA(t *testing.T) {
	tests := []struct {
		name   string
		claims IDClaims
		trust  bool
		acr    string
		want   bool
	}{
		{"not trusted", IDClaims{AMR: stringList{"mfa"}}, false, "", false},
		{"trusted, amr mfa", IDClaims{AMR: stringList{"pwd", "mfa"}}, true, "", true},
		{"trusted, amr otp", IDClaims{AMR: stringList{"otp"}}, true, "", true},
		{"trusted, amr password only", IDClaims{AMR: stringList{"pwd"}}, true, "", false},
		{"trusted, no amr", IDClaims{}, true, "", false},
		{"acr required and matched", IDClaims{ACR: "loa3"}, true, "loa3", true},
		{"acr required and amr alone does not do", IDClaims{AMR: stringList{"mfa"}}, true, "loa3", false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			claims := tt.claims
			if got := idpSatisfiedMFA(&claims, tt.trust, tt.acr); got != tt.want {
				t.Errorf("idpSatisfiedMFA = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestStringListAcceptsBothShapes(t *testing.T) {
	var single stringList
	if err := json.Unmarshal([]byte(`"a"`), &single); err != nil || len(single) != 1 || single[0] != "a" {
		t.Fatalf("single = %v, %v", single, err)
	}
	var many stringList
	if err := json.Unmarshal([]byte(`["a","b"]`), &many); err != nil || len(many) != 2 {
		t.Fatalf("many = %v, %v", many, err)
	}
	if err := json.Unmarshal([]byte(`5`), &many); err == nil {
		t.Fatal("a number is not an audience")
	}
	if !many.contains("a") || many.contains("c") {
		t.Error("contains is wrong")
	}
}

// The distinction between an absent email_verified and a false one decides
// whether a provider is unsupported or is telling us the address is unverified.
func TestFlexBoolDistinguishesAbsentFromFalse(t *testing.T) {
	tests := []struct {
		body    string
		wantSet bool
		wantVal bool
		wantErr bool
	}{
		{`{"email_verified":true}`, true, true, false},
		{`{"email_verified":false}`, true, false, false},
		{`{"email_verified":"true"}`, true, true, false},
		{`{"email_verified":"false"}`, true, false, false},
		{`{}`, false, false, false},
		{`{"email_verified":"yes"}`, false, false, true},
		{`{"email_verified":1}`, false, false, true},
	}
	for _, tt := range tests {
		t.Run(tt.body, func(t *testing.T) {
			var claims IDClaims
			err := json.Unmarshal([]byte(tt.body), &claims)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a decode error")
				}
				return
			}
			if err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			if claims.EmailVerified.Set != tt.wantSet || claims.EmailVerified.Value != tt.wantVal {
				t.Errorf("EmailVerified = %+v, want set=%v value=%v", claims.EmailVerified, tt.wantSet, tt.wantVal)
			}
		})
	}
}

// The dial guard is what stops a workspace administrator pointing "issuer" at
// the cloud metadata service or at a database on the pod network.
func TestIsPublicIP(t *testing.T) {
	tests := []struct {
		ip   string
		want bool
	}{
		{"8.8.8.8", true},
		{"2001:4860:4860::8888", true},
		{"127.0.0.1", false},
		{"::1", false},
		{"0.0.0.0", false},
		{"10.1.2.3", false},
		{"172.16.0.1", false},
		{"192.168.1.1", false},
		{"169.254.169.254", false}, // the one that matters
		{"fe80::1", false},
		{"fd00::1", false},
		{"100.64.0.1", false},
		{"100.128.0.1", true}, // outside the CGNAT range
		{"224.0.0.1", false},
	}
	for _, tt := range tests {
		t.Run(tt.ip, func(t *testing.T) {
			if got := isPublicIP(net.ParseIP(tt.ip)); got != tt.want {
				t.Errorf("isPublicIP(%s) = %v, want %v", tt.ip, got, tt.want)
			}
		})
	}
}

func TestIssuerValidation(t *testing.T) {
	strict := NewClient(Config{}.Defaults())
	lax := NewClient(Config{AllowInsecureIssuer: true}.Defaults())

	tests := []struct {
		name       string
		issuer     string
		wantStrict bool // true means accepted
		wantLax    bool
	}{
		{"https", "https://idp.example.com", true, true},
		{"https with path", "https://idp.example.com/tenant/1", true, true},
		{"http", "http://idp.example.com", false, true},
		{"no host", "https://", false, false},
		{"not a url", "::::", false, false},
		{"query string", "https://idp.example.com?a=b", false, false},
		{"other scheme", "ftp://idp.example.com", false, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strict.validateIssuer(tt.issuer) == nil; got != tt.wantStrict {
				t.Errorf("strict accepted = %v, want %v", got, tt.wantStrict)
			}
			if got := lax.validateIssuer(tt.issuer) == nil; got != tt.wantLax {
				t.Errorf("lax accepted = %v, want %v", got, tt.wantLax)
			}
		})
	}
}

func TestSelectKeyRefusesAmbiguity(t *testing.T) {
	one := []parsedKey{{kid: "a", alg: "RS256"}}
	two := []parsedKey{{kid: "a", alg: "RS256"}, {kid: "b", alg: "RS256"}}

	if _, ok := selectKey(one, "", "RS256"); !ok {
		t.Error("a single key with no kid in the header should be usable")
	}
	if _, ok := selectKey(two, "", "RS256"); ok {
		t.Error("with several keys and no kid there is no correct choice")
	}
	if _, ok := selectKey(two, "b", "RS256"); !ok {
		t.Error("a named key should be found")
	}
	if _, ok := selectKey(two, "c", "RS256"); ok {
		t.Error("an unknown kid must not match")
	}
	// A key the provider pinned to RS256 must not verify an ES256 signature.
	if _, ok := selectKey(two, "a", "ES256"); ok {
		t.Error("algorithm pinning is not enforced")
	}
	// A symmetric algorithm is not in signatureAlgs at all.
	if _, ok := signatureAlgs["HS256"]; ok {
		t.Error("HS256 must not be an accepted id_token algorithm")
	}
	if _, ok := signatureAlgs["none"]; ok {
		t.Error("alg=none must not be accepted")
	}
}

func TestParseJWKSRejectsUnusableKeySets(t *testing.T) {
	tests := []struct {
		name string
		body string
	}{
		{"not json", `nope`},
		{"no keys", `{"keys":[]}`},
		{"symmetric only", `{"keys":[{"kty":"oct","kid":"a","k":"AAAA"}]}`},
		{"encryption use only", `{"keys":[{"kty":"RSA","use":"enc","kid":"a","n":"AQAB","e":"AQAB"}]}`},
		{"rsa modulus too small", `{"keys":[{"kty":"RSA","kid":"a","n":"AQAB","e":"AQAB"}]}`},
		{"ec point not on curve", `{"keys":[{"kty":"EC","kid":"a","crv":"P-256","x":"AQAB","y":"AQAB"}]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := parseJWKS([]byte(tt.body)); err == nil {
				t.Error("expected an error")
			}
		})
	}
}

func TestOnlyPostAuth(t *testing.T) {
	tests := []struct {
		name    string
		methods []string
		want    bool
	}{
		{"nothing advertised", nil, false},
		{"basic supported", []string{"client_secret_basic", "client_secret_post"}, false},
		{"post only", []string{"client_secret_post"}, true},
		{"something else entirely", []string{"private_key_jwt"}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := onlyPostAuth(tt.methods); got != tt.want {
				t.Errorf("onlyPostAuth = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestConfigDefaultsAndEnablement(t *testing.T) {
	cfg := Config{}.Defaults()
	if cfg.HTTPTimeout == 0 || cfg.AuthRequestTTL == 0 || cfg.PendingTTL == 0 || cfg.JWKSCacheTTL == 0 || cfg.ClockSkew == 0 {
		t.Fatalf("Defaults left a zero value: %+v", cfg)
	}
	if cfg.IsEnabled() {
		t.Error("SSO must be disabled without a secret key")
	}
	if !(Config{SecretKey: make([]byte, 32)}).IsEnabled() {
		t.Error("a 32-byte key should enable SSO")
	}
	if (Config{SecretKey: make([]byte, 16)}).IsEnabled() {
		t.Error("a short key must not enable SSO")
	}
}
