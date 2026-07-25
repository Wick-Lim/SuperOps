package app

import (
	"strings"
	"testing"
	"time"
)

func TestSSOIsEnabled(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"unset", "", false},
		{"whitespace only", "   ", false},
		{"set", strings.Repeat("a", 64), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := (SSOConfig{SecretKey: tt.key}).IsEnabled(); got != tt.want {
				t.Errorf("IsEnabled() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestLoadConfigSSODefaults(t *testing.T) {
	validEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.SSO.IsEnabled() {
		t.Error("SSO must be off without SSO_SECRET_KEY: client secrets have nothing to be sealed with")
	}
	for _, tc := range []struct {
		name string
		got  time.Duration
		want time.Duration
	}{
		{"SSO_HTTP_TIMEOUT", cfg.SSO.HTTPTimeout, 10 * time.Second},
		{"SSO_AUTH_REQUEST_TTL", cfg.SSO.AuthRequestTTL, 10 * time.Minute},
		{"SSO_PENDING_TTL", cfg.SSO.PendingTTL, 5 * time.Minute},
		{"SSO_JWKS_CACHE_TTL", cfg.SSO.JWKSCacheTTL, time.Hour},
		{"SSO_CLOCK_SKEW", cfg.SSO.ClockSkew, 2 * time.Minute},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %s, want %s", tc.name, tc.got, tc.want)
		}
	}
}

func TestLoadConfigRejectsMalformedSSOSecretKey(t *testing.T) {
	// A key that decodes to the wrong bytes is silent: it "works" until the
	// next process starts with a different guess and cannot open what the
	// previous one sealed. So it fails the boot instead.
	tests := []struct {
		name string
		key  string
	}{
		{"too short", "not-a-key"},
		{"31 raw characters", strings.Repeat("k", 31)},
		{"odd-length hex", strings.Repeat("a", 63)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SSO_SECRET_KEY", tt.key)

			if _, err := LoadConfig(); err == nil {
				t.Fatalf("expected SSO_SECRET_KEY=%q to fail startup", tt.key)
			} else if !strings.Contains(err.Error(), "SSO_SECRET_KEY") {
				t.Errorf("error %q does not name SSO_SECRET_KEY", err)
			}
		})
	}
}

func TestLoadConfigAcceptsEverySSOSecretKeyFormat(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{"64 hex characters", strings.Repeat("ab", 32)},
		{"base64 of 32 bytes", "MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY="},
		{"32 raw characters", strings.Repeat("k", 32)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SSO_SECRET_KEY", tt.key)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("LoadConfig: %v", err)
			}
			if !cfg.SSO.IsEnabled() {
				t.Error("SSO_SECRET_KEY is set but IsEnabled() is false")
			}
		})
	}
}

// TestLoadConfigRefusesInsecureIssuerOnRealDeployment pins the decision made in
// validateSSO: the flag drops the https requirement AND the private-address
// guard on a URL supplied by a workspace administrator — i.e. a tenant — so
// leaving it on in production is an SSRF primitive against the node's metadata
// service, not a lint. A Warn nobody reads is not a control.
func TestLoadConfigRefusesInsecureIssuerOnRealDeployment(t *testing.T) {
	for _, baseURL := range []string{
		"https://chat.example.com",
		"http://chat.example.com:8080",
	} {
		t.Run(baseURL, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SSO_SECRET_KEY", strings.Repeat("ab", 32))
			t.Setenv("SSO_ALLOW_INSECURE_ISSUER", "true")
			t.Setenv("PUBLIC_BASE_URL", baseURL)

			_, err := LoadConfig()
			if err == nil {
				t.Fatal("expected SSO_ALLOW_INSECURE_ISSUER=true to fail startup on a non-loopback deployment")
			}
			if !strings.Contains(err.Error(), "SSO_ALLOW_INSECURE_ISSUER") {
				t.Errorf("error %q does not name SSO_ALLOW_INSECURE_ISSUER", err)
			}
		})
	}
}

// It is refused even with SSO switched off: the variable is inert today, but it
// is exactly the leftover that goes live the day someone sets SSO_SECRET_KEY.
func TestLoadConfigRefusesInsecureIssuerEvenWithSSODisabled(t *testing.T) {
	validEnv(t)
	t.Setenv("SSO_ALLOW_INSECURE_ISSUER", "true")
	t.Setenv("PUBLIC_BASE_URL", "https://chat.example.com")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("expected SSO_ALLOW_INSECURE_ISSUER=true to fail startup even with SSO disabled")
	}
}

func TestLoadConfigAllowsInsecureIssuerOnLoopbackDeployment(t *testing.T) {
	for _, baseURL := range []string{
		"http://localhost:8080",
		"http://127.0.0.1:8080",
		"http://[::1]:8080",
	} {
		t.Run(baseURL, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SSO_SECRET_KEY", strings.Repeat("ab", 32))
			t.Setenv("SSO_ALLOW_INSECURE_ISSUER", "true")
			t.Setenv("PUBLIC_BASE_URL", baseURL)

			cfg, err := LoadConfig()
			if err != nil {
				t.Fatalf("a local test deployment must still be able to point at a local IdP: %v", err)
			}
			if !cfg.SSO.AllowInsecureIssuer {
				t.Error("SSO_ALLOW_INSECURE_ISSUER=true did not survive into the config")
			}
		})
	}
}

func TestLoadConfigRejectsNonPositiveSSODurations(t *testing.T) {
	for _, key := range []string{
		"SSO_HTTP_TIMEOUT",
		"SSO_AUTH_REQUEST_TTL",
		"SSO_PENDING_TTL",
		"SSO_JWKS_CACHE_TTL",
		"SSO_CLOCK_SKEW",
	} {
		t.Run(key, func(t *testing.T) {
			validEnv(t)
			t.Setenv("SSO_SECRET_KEY", strings.Repeat("ab", 32))
			t.Setenv(key, "0s")

			_, err := LoadConfig()
			if err == nil {
				t.Fatalf("expected %s=0s to fail startup", key)
			}
			if !strings.Contains(err.Error(), key) {
				t.Errorf("error %q does not name %s", err, key)
			}
		})
	}
}
