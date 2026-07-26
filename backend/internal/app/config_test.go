package app

import (
	"net/url"
	"strings"
	"testing"
	"time"
)

// validEnv is the minimum that makes LoadConfig succeed, so each test can vary
// exactly one variable.
func validEnv(t *testing.T) {
	t.Helper()
	t.Setenv("JWT_SECRET", strings.Repeat("k", MinJWTSecretBytes))
	t.Setenv("ADMIN_EMAIL", "admin@example.com")
	t.Setenv("ADMIN_PASSWORD", "correct horse battery staple")
}

func TestLoadConfigRejectsMalformedValues(t *testing.T) {
	tests := []struct {
		name      string
		key       string
		value     string
		wantInErr string
	}{
		{"non-numeric port", "SERVER_PORT", "eighty", "SERVER_PORT"},
		{"non-numeric max conns", "DB_MAX_CONNS", "many", "DB_MAX_CONNS"},
		{"non-boolean rate limit switch", "RATE_LIMIT_ENABLED", "yes", "RATE_LIMIT_ENABLED"},
		{"non-boolean search switch", "SEARCH_ENABLED", "on", "SEARCH_ENABLED"},
		{"invalid duration", "JWT_ACCESS_TTL", "15min", "JWT_ACCESS_TTL"},
		{"invalid db statement timeout", "DB_STATEMENT_TIMEOUT", "30 seconds", "DB_STATEMENT_TIMEOUT"},
		{"invalid redis pool size", "REDIS_POOL_SIZE", "3.5", "REDIS_POOL_SIZE"},
		{"invalid proxy hops", "RATE_LIMIT_TRUSTED_PROXY_HOPS", "one", "RATE_LIMIT_TRUSTED_PROXY_HOPS"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv(tt.key, tt.value)

			cfg, err := LoadConfig()
			if err == nil {
				t.Fatalf("expected %s=%q to fail startup, got config %+v", tt.key, tt.value, cfg)
			}
			if !strings.Contains(err.Error(), tt.wantInErr) {
				t.Errorf("error %q does not name %s", err, tt.wantInErr)
			}
		})
	}
}

func TestLoadConfigReportsEveryMalformedValue(t *testing.T) {
	validEnv(t)
	t.Setenv("SERVER_PORT", "eighty")
	t.Setenv("REDIS_DB", "zero")

	_, err := LoadConfig()
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, key := range []string{"SERVER_PORT", "REDIS_DB"} {
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q omits %s; operators should see all of them at once", err, key)
		}
	}
}

func TestLoadConfigAcceptsWellFormedValues(t *testing.T) {
	validEnv(t)
	t.Setenv("SERVER_PORT", "9090")
	t.Setenv("RATE_LIMIT_ENABLED", "false")
	t.Setenv("JWT_ACCESS_TTL", "5m")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Server.Port != 9090 {
		t.Errorf("port = %d, want 9090", cfg.Server.Port)
	}
	if cfg.RateLimit.Enabled {
		t.Error("rate limiting should be disabled")
	}
	if cfg.JWT.AccessTokenTTL != 5*time.Minute {
		t.Errorf("access TTL = %v, want 5m", cfg.JWT.AccessTokenTTL)
	}
}

func TestLoadConfigJWTSecretLength(t *testing.T) {
	tests := []struct {
		name    string
		secret  string
		wantErr bool
	}{
		{"empty", "", true},
		{"one byte short", strings.Repeat("k", MinJWTSecretBytes-1), true},
		{"exactly the minimum", strings.Repeat("k", MinJWTSecretBytes), false},
		{"longer", strings.Repeat("k", 64), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv("JWT_SECRET", tt.secret)

			_, err := LoadConfig()
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected a startup failure")
				}
				if !strings.Contains(err.Error(), "JWT_SECRET") {
					t.Errorf("error %q does not name JWT_SECRET", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
		})
	}
}

func TestLoadConfigRateLimitProxyDefaults(t *testing.T) {
	validEnv(t)

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	// Trusting X-Forwarded-For by default is what made the login limiter
	// bypassable with a single header.
	if cfg.RateLimit.TrustProxy {
		t.Error("RATE_LIMIT_TRUST_PROXY must default to false")
	}
	if cfg.RateLimit.TrustedProxyHops != 0 {
		t.Errorf("RATE_LIMIT_TRUSTED_PROXY_HOPS default = %d, want 0", cfg.RateLimit.TrustedProxyHops)
	}
}

func TestLoadConfigRejectsHopsWithoutTrust(t *testing.T) {
	validEnv(t)
	t.Setenv("RATE_LIMIT_TRUSTED_PROXY_HOPS", "1")

	if _, err := LoadConfig(); err == nil {
		t.Fatal("configuring hops without RATE_LIMIT_TRUST_PROXY should fail loudly, not be ignored")
	}
}

func TestLoadConfigRejectsOutOfRangeValues(t *testing.T) {
	tests := []struct {
		name string
		key  string
		val  string
	}{
		{"port zero", "SERVER_PORT", "0"},
		{"port too high", "SERVER_PORT", "70000"},
		{"no connections", "DB_MAX_CONNS", "0"},
		{"min above max", "DB_MIN_CONNS", "999"},
		{"zero api limit", "RATE_LIMIT_API_PER_MIN", "0"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			validEnv(t)
			t.Setenv(tt.key, tt.val)

			if _, err := LoadConfig(); err == nil {
				t.Fatalf("%s=%s should fail validation", tt.key, tt.val)
			}
		})
	}
}

func TestSearchAndFilesCanBeDisabled(t *testing.T) {
	validEnv(t)
	t.Setenv("SEARCH_ENABLED", "false")
	t.Setenv("FILES_ENABLED", "false")

	cfg, err := LoadConfig()
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}
	if cfg.Meili.IsEnabled() {
		t.Error("SEARCH_ENABLED=false must disable search")
	}
	if cfg.MinIO.IsEnabled() {
		t.Error("FILES_ENABLED=false must disable files")
	}
}

func TestOptionalFeatureGuards(t *testing.T) {
	// MEILI_HOST has a non-empty default, so `Host != ""` alone could never
	// turn search off — that is why the explicit switch exists. Both conditions
	// still have to hold.
	meili := []struct {
		cfg  MeiliConfig
		want bool
	}{
		{MeiliConfig{Enabled: true, Host: "http://meili:7700"}, true},
		{MeiliConfig{Enabled: false, Host: "http://meili:7700"}, false},
		{MeiliConfig{Enabled: true, Host: ""}, false},
	}
	for _, tt := range meili {
		if got := tt.cfg.IsEnabled(); got != tt.want {
			t.Errorf("MeiliConfig%+v.IsEnabled() = %v, want %v", tt.cfg, got, tt.want)
		}
	}

	minio := []struct {
		cfg  MinIOConfig
		want bool
	}{
		{MinIOConfig{Enabled: true, Endpoint: "minio:9000"}, true},
		{MinIOConfig{Enabled: false, Endpoint: "minio:9000"}, false},
		{MinIOConfig{Enabled: true, Endpoint: ""}, false},
	}
	for _, tt := range minio {
		if got := tt.cfg.IsEnabled(); got != tt.want {
			t.Errorf("MinIOConfig%+v.IsEnabled() = %v, want %v", tt.cfg, got, tt.want)
		}
	}
}

func TestDBConfigDSN(t *testing.T) {
	base := DBConfig{
		Host:             "db.internal",
		Port:             5432,
		User:             "superops",
		Password:         "p@ss w/ord",
		Name:             "superops",
		SSLMode:          "require",
		ApplicationName:  "superops",
		StatementTimeout: 30 * time.Second,
	}

	dsn := base.DSN()
	u, err := url.Parse(dsn)
	if err != nil {
		t.Fatalf("generated DSN %q does not parse: %v", dsn, err)
	}
	if u.Host != "db.internal:5432" {
		t.Errorf("host = %q", u.Host)
	}
	pw, _ := u.User.Password()
	if pw != "p@ss w/ord" {
		t.Errorf("password round-trip = %q; special characters must be escaped", pw)
	}
	q := u.Query()
	if q.Get("sslmode") != "require" {
		t.Errorf("sslmode = %q", q.Get("sslmode"))
	}
	if q.Get("application_name") != "superops" {
		t.Errorf("application_name = %q", q.Get("application_name"))
	}
	if q.Get("statement_timeout") != "30000" {
		t.Errorf("statement_timeout = %q, want milliseconds", q.Get("statement_timeout"))
	}
}

func TestDBConfigDSNURLOverride(t *testing.T) {
	c := DBConfig{
		Host:             "ignored.internal",
		Port:             5432,
		User:             "ignored",
		Name:             "ignored",
		SSLMode:          "disable",
		URL:              "postgres://ro:pw@replica-a:5432,replica-b:5432/superops?target_session_attrs=any",
		ApplicationName:  "superops-worker",
		StatementTimeout: 10 * time.Second,
	}

	dsn := c.DSN()
	if !strings.Contains(dsn, "replica-a:5432,replica-b:5432") {
		t.Fatalf("DATABASE_URL must win outright, got %q", dsn)
	}
	if strings.Contains(dsn, "ignored") {
		t.Errorf("discrete fields leaked into the override: %q", dsn)
	}

	u, _ := url.Parse(dsn)
	q := u.Query()
	if q.Get("target_session_attrs") != "any" {
		t.Errorf("operator query params must be preserved, got %q", dsn)
	}
	if q.Get("application_name") != "superops-worker" {
		t.Errorf("application_name = %q", q.Get("application_name"))
	}
	if q.Get("statement_timeout") != "10000" {
		t.Errorf("statement_timeout = %q", q.Get("statement_timeout"))
	}
}

func TestDBConfigDSNDoesNotOverrideExplicitParams(t *testing.T) {
	c := DBConfig{
		URL:              "postgres://u:p@h:5432/db?application_name=custom&statement_timeout=1234",
		ApplicationName:  "superops",
		StatementTimeout: 30 * time.Second,
	}

	u, err := url.Parse(c.DSN())
	if err != nil {
		t.Fatal(err)
	}
	q := u.Query()
	if q.Get("application_name") != "custom" {
		t.Errorf("application_name = %q, want the operator's value", q.Get("application_name"))
	}
	if q.Get("statement_timeout") != "1234" {
		t.Errorf("statement_timeout = %q, want the operator's value", q.Get("statement_timeout"))
	}
}

func TestDBConfigDSNKeywordValueFormPassedThrough(t *testing.T) {
	c := DBConfig{
		URL:             "host=/var/run/postgresql user=superops dbname=superops",
		ApplicationName: "superops",
	}
	if got := c.DSN(); got != c.URL {
		t.Errorf("keyword/value DSN was rewritten to %q; pgx accepts this form verbatim", got)
	}
}

// A HALF-CONFIGURED MEDIA SERVER MUST FAIL THE BOOT.
//
// RTCConfig.IsEnabled() returns false when any of the three is missing, so a
// deployment that set RTC_HOST and forgot RTC_API_SECRET got huddles silently
// ABSENT — and a log line saying "no media server configured" about a
// configuration that plainly names one. Every other deployment-dependent
// capability here fails loudly; this was the exception.
func TestPartialRTCConfigurationFailsTheBoot(t *testing.T) {
	base := func(t *testing.T) {
		t.Helper()
		t.Setenv("JWT_SECRET", "a_jwt_secret_that_is_long_enough_to_pass")
		t.Setenv("ADMIN_EMAIL", "admin@company.com")
		t.Setenv("ADMIN_PASSWORD", "changeme_admin_password")
		t.Setenv("PUBLIC_BASE_URL", "http://localhost:8080")
	}

	t.Run("host without a secret is refused", func(t *testing.T) {
		base(t)
		t.Setenv("RTC_HOST", "http://sfu:7880")
		t.Setenv("RTC_API_KEY", "devkey")
		if _, err := LoadConfig(); err == nil {
			t.Fatal("loaded a configuration that names a media server it cannot authenticate to")
		} else if !strings.Contains(err.Error(), "RTC_API_SECRET") {
			t.Errorf("the error does not name the missing variable: %v", err)
		}
	})

	t.Run("nothing set at all is fine", func(t *testing.T) {
		base(t)
		if _, err := LoadConfig(); err != nil {
			t.Fatalf("a deployment with no media server must boot: %v", err)
		}
	})

	t.Run("all three set is fine", func(t *testing.T) {
		base(t)
		t.Setenv("RTC_HOST", "http://sfu:7880")
		t.Setenv("RTC_API_KEY", "devkey")
		t.Setenv("RTC_API_SECRET", "devsecret-at-least-32-characters!!")
		cfg, err := LoadConfig()
		if err != nil {
			t.Fatalf("a complete configuration was refused: %v", err)
		}
		if !cfg.RTC.IsEnabled() {
			t.Error("a complete configuration reports itself disabled")
		}
	})

	// An open relay on the customer's network is not a configuration mistake to
	// discover in production.
	t.Run("TURN without a shared secret is refused", func(t *testing.T) {
		base(t)
		t.Setenv("RTC_ICE_URLS", "turn:relay.example:3478")
		if _, err := LoadConfig(); err == nil {
			t.Fatal("loaded a configuration that would serve an open relay")
		}
	})

	t.Run("STUN alone needs no secret", func(t *testing.T) {
		base(t)
		t.Setenv("RTC_ICE_URLS", "stun:stun.example:3478")
		if _, err := LoadConfig(); err != nil {
			t.Fatalf("STUN-only was refused: %v", err)
		}
	})
}
