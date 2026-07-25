package app

import (
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/sso"
)

type Config struct {
	Server       ServerConfig
	DB           DBConfig
	Redis        RedisConfig
	NATS         NATSConfig
	JWT          JWTConfig
	MinIO        MinIOConfig
	Meili        MeiliConfig
	Admin        AdminConfig
	RateLimit    RateLimitConfig
	CORS         CORSConfig
	Push         PushConfig
	Mail         MailConfig
	SSO          SSOConfig
	Inbox        InboxConfig
	Audit        AuditConfig
	MetricsToken string // METRICS_TOKEN — if set, GET /metrics requires this bearer token
	LogLevel     string
}

// MailConfig configures outbound email.
//
// The transport is deployment policy, not a code decision, which is why it is
// here rather than compiled in. See internal/mail's package comment: almost
// every cloud provider blocks outbound port 25, so a cloud deployment must
// relay through a provider, while an on-premises host that allows 25 can
// deliver directly and keep company mail off a third party.
//
// The default is TransportLog: a fresh deployment must not be able to mail real
// people before an operator has opted in.
type MailConfig struct {
	// Transport is one of mail.TransportLog (default), mail.TransportSMTP,
	// mail.TransportSMTPDirect, mail.TransportResend.
	Transport string

	From     string // MAIL_FROM — the envelope and header sender
	FromName string // MAIL_FROM_NAME — optional display name

	// PublicBaseURL is the absolute, publicly reachable root of the web client.
	// Every link in every message is built from it; a relative "/invite/<token>"
	// in an email is not a link.
	PublicBaseURL string

	// ProductName appears in subjects and in the message layout.
	ProductName string

	// Timeout bounds one send attempt.
	Timeout time.Duration

	// TestPerMinute rate-limits POST /api/v1/admin/mail/test per IP. It is an
	// outbound-mail trigger, so it gets its own (very low) budget rather than
	// the general API limit.
	TestPerMinute int

	SMTP   MailSMTPConfig
	Direct MailDirectConfig
	Resend MailResendConfig
}

// MailSMTPConfig is the authenticated-relay transport.
//
// One transport, every provider: SES, Postmark, SendGrid, Mailgun, Resend and a
// corporate Exchange relay all expose authenticated SMTP. Only Host, Username
// and Password change between them — there is no per-provider code path.
type MailSMTPConfig struct {
	Host     string
	Port     int
	Username string
	Password string

	// TLS is mail.TLSStartTLS (587, the default), mail.TLSImplicit (465) or
	// mail.TLSNone.
	TLS string

	// AllowInsecureAuth permits credentials over an unencrypted connection. Only
	// for a relay on localhost or a trusted private network.
	AllowInsecureAuth bool

	// InsecureSkipVerify disables certificate verification, for a relay whose
	// private CA is not in the image's trust store.
	InsecureSkipVerify bool

	HELO string
}

// MailDirectConfig is MX delivery, for on-premises only.
type MailDirectConfig struct {
	// HELO must be this host's public FQDN. Receiving MTAs check it against the
	// connecting IP's PTR record.
	HELO string
	Port int

	DKIMDomain   string
	DKIMSelector string

	// DKIMPrivateKey is a PEM-encoded RSA key, or its base64 encoding for
	// environments that cannot carry newlines in a variable.
	DKIMPrivateKey string

	// DKIMPrivateKeyFile reads the key from disk instead — the right shape for a
	// mounted Kubernetes Secret. Wins over DKIMPrivateKey when both are set.
	DKIMPrivateKeyFile string
}

// MailResendConfig is the HTTP transport, for hosts where even the SMTP
// submission ports are blocked outbound.
type MailResendConfig struct {
	APIKey   string
	Endpoint string
}

type CORSConfig struct {
	AllowedOrigins []string
}

type RateLimitConfig struct {
	Enabled       bool
	APIPerMinute  int // general authenticated API ceiling
	AuthPerMinute int // strict limit for login/refresh/accept-invite (per IP)

	// TrustProxy honors X-Forwarded-For. Defaults to FALSE: with it on and no
	// proxy in front, any client picks its own rate-limit bucket by sending the
	// header, which defeats the login brute-force limit outright.
	TrustProxy bool

	// TrustedProxyHops is how many reverse proxies sit in front of this process
	// (1 for the reference nginx). The client address is read as the Nth entry
	// from the RIGHT of X-Forwarded-For, since every proxy appends the peer it
	// saw and everything to the left of that is client-supplied.
	TrustedProxyHops int
}

type AdminConfig struct {
	Email    string
	Password string
	Username string
}

type ServerConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

type DBConfig struct {
	// URL is an optional full connection string (DATABASE_URL). When set it
	// wins over the discrete fields below.
	URL string

	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
	MaxConns int32
	MinConns int32

	ApplicationName          string
	StatementTimeout         time.Duration
	LockTimeout              time.Duration
	IdleInTransactionTimeout time.Duration
}

// DSN renders the connection string handed to pgx.
//
// DATABASE_URL wins outright when set. The discrete host/port fields cannot
// express a multi-host failover list, a pooler endpoint, or
// target_session_attrs — which is why the provisioned read replica received no
// traffic: there was no way to name it.
//
// Either way the result carries application_name (so pg_stat_activity
// attributes load to a service rather than to "unknown") and statement_timeout
// (so one runaway query cannot pin a pool slot indefinitely). Values already
// present in the URL are left alone — an explicit operator setting wins.
func (c DBConfig) DSN() string {
	raw := c.URL
	if raw == "" {
		u := &url.URL{
			Scheme:   "postgres",
			User:     url.UserPassword(c.User, c.Password),
			Host:     net.JoinHostPort(c.Host, strconv.Itoa(c.Port)),
			Path:     "/" + c.Name,
			RawQuery: url.Values{"sslmode": {c.SSLMode}}.Encode(),
		}
		raw = u.String()
	}
	return c.decorateDSN(raw)
}

func (c DBConfig) decorateDSN(raw string) string {
	// pgx also accepts the keyword/value form ("host=... user=..."); leave that
	// verbatim rather than mangling it.
	if !strings.HasPrefix(raw, "postgres://") && !strings.HasPrefix(raw, "postgresql://") {
		return raw
	}

	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}

	q := u.Query()
	if c.ApplicationName != "" && q.Get("application_name") == "" {
		q.Set("application_name", c.ApplicationName)
	}
	if c.StatementTimeout > 0 && q.Get("statement_timeout") == "" {
		q.Set("statement_timeout", strconv.FormatInt(c.StatementTimeout.Milliseconds(), 10))
	}
	u.RawQuery = q.Encode()

	return u.String()
}

type RedisConfig struct {
	Addr         string
	Password     string
	DB           int
	DialTimeout  time.Duration
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	PoolSize     int
}

type NATSConfig struct {
	URL          string
	DrainTimeout time.Duration
}

type JWTConfig struct {
	Secret          string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

// MinJWTSecretBytes is the shortest secret accepted. HS256 keys shorter than
// the 256-bit digest add no security over a 256-bit one and are usually a
// leftover placeholder.
const MinJWTSecretBytes = 32

type MinIOConfig struct {
	Enabled   bool
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

// IsEnabled reports whether the file feature should be constructed.
func (c MinIOConfig) IsEnabled() bool { return c.Enabled && c.Endpoint != "" }

// PushConfig configures mobile push notifications via Expo's push service.
//
// Enabled defaults to FALSE, unlike files and search. Push sends the first 140
// characters of every message to a third party (Expo, and through it Apple and
// Google), which is a decision an operator has to make deliberately — not one
// they discover after the fact because the default was on.
type PushConfig struct {
	Enabled bool

	// Endpoint overrides Expo's push API. Empty means the package default; it
	// exists so a self-hosted relay or a test double can be pointed at.
	Endpoint string

	// AccessToken is an Expo access token. Required only when the Expo project
	// has "enhanced push security" enabled; a secret either way.
	AccessToken string

	// Timeout bounds one request to the push service.
	Timeout time.Duration

	// QueueSize and Workers size the in-process dispatcher. Zero means the
	// push package's defaults.
	QueueSize int
	Workers   int
}

// IsEnabled reports whether the push pipeline should be constructed. It exists
// for symmetry with MinIOConfig/MeiliConfig, so no caller tests the raw field.
func (c PushConfig) IsEnabled() bool { return c.Enabled }

// InboxConfig tunes the unified inbox's batched email digest. Both windows are
// deployment policy rather than product design: a support team wants a tighter
// loop than a company that mails once a day.
type InboxConfig struct {
	// DigestQuietPeriod is how long an item must sit untouched before it is
	// mailable. It is what collapses a burst — an item still receiving events is
	// pushed back out of the window on every new event.
	DigestQuietPeriod time.Duration
	// DigestMinInterval is the hard ceiling on mail volume per person per
	// workspace, and it also gates email='immediate' so choosing that cannot
	// exceed the digest's budget.
	DigestMinInterval time.Duration
}

// AuditConfig covers retention and off-box anchoring.
//
// The Sink is a §3c deployment-dependent capability: one interface, transport by
// configuration, validated at boot, safe default. See internal/audit/sink.go for
// why the default ('log') is useful rather than a placeholder and why an `s3`
// transport was cut.
type AuditConfig struct {
	// RetentionDays is deployment-wide. Enforced by DROPPING PARTITIONS, not by
	// a batched DELETE. Zero disables retention.
	RetentionDays int

	// Sink is one of audit.SinkLog (default), audit.SinkFile, audit.SinkHTTP.
	Sink         string
	SinkPath     string // AUDIT_SINK=file
	SinkEndpoint string // AUDIT_SINK=http
	SinkSecret   string // AUDIT_SINK=http — HMAC key; required, see sink.go
	SinkTimeout  time.Duration

	// BufferSize and BufferWorkers size the Tier 2 queue (egress and sensitive
	// reads). Zero means the audit package's defaults.
	BufferSize    int
	BufferWorkers int

	// ExportPerMinute is the per-IP budget for GET /admin/audit-logs/export and
	// POST /admin/audit-sink/test. Both are one-request-a-lot-of-consequence
	// endpoints, which is the same argument MAIL_TEST_PER_MIN makes.
	ExportPerMinute int
}

type MeiliConfig struct {
	Enabled   bool
	Host      string
	MasterKey string
}

// IsEnabled reports whether the search feature should be constructed.
//
// Callers must use this instead of `Host != ""`: MEILI_HOST has a non-empty
// default, so the old guard was always true and setting MEILI_HOST="" to
// disable search did the opposite of what it looked like.
func (c MeiliConfig) IsEnabled() bool { return c.Enabled && c.Host != "" }

// SSOConfig configures OpenID Connect single sign-on.
//
// Per-workspace provider configuration lives in Postgres (one deployment serves
// several organizations); this is the deployment-level half.
//
// SecretKey is the on/off switch: without it, client secrets cannot be sealed
// at rest, so the whole feature refuses to construct rather than storing them
// in the clear.
type SSOConfig struct {
	SecretKey           string        // SSO_SECRET_KEY — 32 bytes: 64 hex chars, base64, or 32 raw chars
	AllowInsecureIssuer bool          // SSO_ALLOW_INSECURE_ISSUER — dev/test only
	HTTPTimeout         time.Duration // SSO_HTTP_TIMEOUT       (default 10s)
	AuthRequestTTL      time.Duration // SSO_AUTH_REQUEST_TTL   (default 10m)
	PendingTTL          time.Duration // SSO_PENDING_TTL        (default 5m)
	JWKSCacheTTL        time.Duration // SSO_JWKS_CACHE_TTL     (default 1h)
	ClockSkew           time.Duration // SSO_CLOCK_SKEW         (default 2m)
}

// IsEnabled reports whether the SSO stack should be constructed. Mirrors
// MinIOConfig.IsEnabled / PushConfig.IsEnabled so no caller tests a raw field.
func (c SSOConfig) IsEnabled() bool { return strings.TrimSpace(c.SecretKey) != "" }

func LoadConfig() (*Config, error) {
	e := &env{}

	cfg := &Config{
		Server: ServerConfig{
			Host:         e.str("SERVER_HOST", "0.0.0.0"),
			Port:         e.int("SERVER_PORT", 8080),
			ReadTimeout:  e.duration("SERVER_READ_TIMEOUT", 15*time.Second),
			WriteTimeout: e.duration("SERVER_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:  e.duration("SERVER_IDLE_TIMEOUT", 60*time.Second),
		},
		DB: DBConfig{
			URL:                      e.str("DATABASE_URL", ""),
			Host:                     e.str("DB_HOST", "localhost"),
			Port:                     e.int("DB_PORT", 5432),
			User:                     e.str("DB_USER", "superops"),
			Password:                 e.str("DB_PASSWORD", "superops"),
			Name:                     e.str("DB_NAME", "superops"),
			SSLMode:                  e.str("DB_SSLMODE", "disable"),
			MaxConns:                 int32(e.int("DB_MAX_CONNS", 25)),
			MinConns:                 int32(e.int("DB_MIN_CONNS", 5)),
			ApplicationName:          e.str("DB_APPLICATION_NAME", "superops"),
			StatementTimeout:         e.duration("DB_STATEMENT_TIMEOUT", 30*time.Second),
			LockTimeout:              e.duration("DB_LOCK_TIMEOUT", 5*time.Second),
			IdleInTransactionTimeout: e.duration("DB_IDLE_IN_TX_TIMEOUT", 60*time.Second),
		},
		Redis: RedisConfig{
			Addr:         e.str("REDIS_ADDR", "localhost:6379"),
			Password:     e.str("REDIS_PASSWORD", ""),
			DB:           e.int("REDIS_DB", 0),
			DialTimeout:  e.duration("REDIS_DIAL_TIMEOUT", 2*time.Second),
			ReadTimeout:  e.duration("REDIS_READ_TIMEOUT", time.Second),
			WriteTimeout: e.duration("REDIS_WRITE_TIMEOUT", time.Second),
			PoolSize:     e.int("REDIS_POOL_SIZE", 20),
		},
		NATS: NATSConfig{
			URL:          e.str("NATS_URL", "nats://localhost:4222"),
			DrainTimeout: e.duration("NATS_DRAIN_TIMEOUT", 10*time.Second),
		},
		JWT: JWTConfig{
			Secret:          e.str("JWT_SECRET", ""),
			AccessTokenTTL:  e.duration("JWT_ACCESS_TTL", 15*time.Minute),
			RefreshTokenTTL: e.duration("JWT_REFRESH_TTL", 30*24*time.Hour),
		},
		MinIO: MinIOConfig{
			Enabled:   e.bool("FILES_ENABLED", true),
			Endpoint:  e.str("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey: e.str("MINIO_ACCESS_KEY", "minioadmin"),
			SecretKey: e.str("MINIO_SECRET_KEY", "minioadmin"),
			Bucket:    e.str("MINIO_BUCKET", "superops"),
			UseSSL:    e.bool("MINIO_USE_SSL", false),
		},
		Meili: MeiliConfig{
			Enabled:   e.bool("SEARCH_ENABLED", true),
			Host:      e.str("MEILI_HOST", "http://localhost:7700"),
			MasterKey: e.str("MEILI_MASTER_KEY", ""),
		},
		Admin: AdminConfig{
			Email:    e.str("ADMIN_EMAIL", ""),
			Password: e.str("ADMIN_PASSWORD", ""),
			Username: e.str("ADMIN_USERNAME", "admin"),
		},
		RateLimit: RateLimitConfig{
			Enabled:          e.bool("RATE_LIMIT_ENABLED", true),
			APIPerMinute:     e.int("RATE_LIMIT_API_PER_MIN", 600),
			AuthPerMinute:    e.int("RATE_LIMIT_AUTH_PER_MIN", 10),
			TrustProxy:       e.bool("RATE_LIMIT_TRUST_PROXY", false),
			TrustedProxyHops: e.int("RATE_LIMIT_TRUSTED_PROXY_HOPS", 0),
		},
		CORS: CORSConfig{
			AllowedOrigins: e.list("CORS_ALLOWED_ORIGINS", []string{"*"}),
		},
		Push: PushConfig{
			Enabled:     e.bool("PUSH_ENABLED", false),
			Endpoint:    e.str("EXPO_PUSH_ENDPOINT", ""),
			AccessToken: e.str("EXPO_ACCESS_TOKEN", ""),
			Timeout:     e.duration("PUSH_TIMEOUT", 15*time.Second),
			QueueSize:   e.int("PUSH_QUEUE_SIZE", 0),
			Workers:     e.int("PUSH_WORKERS", 0),
		},
		Mail: MailConfig{
			Transport:     strings.ToLower(strings.TrimSpace(e.str("MAIL_TRANSPORT", mail.TransportLog))),
			From:          e.str("MAIL_FROM", ""),
			FromName:      e.str("MAIL_FROM_NAME", "SuperOps"),
			PublicBaseURL: e.str("PUBLIC_BASE_URL", "http://localhost:8080"),
			ProductName:   e.str("PRODUCT_NAME", "SuperOps"),
			Timeout:       e.duration("MAIL_TIMEOUT", 30*time.Second),
			TestPerMinute: e.int("MAIL_TEST_PER_MIN", 3),
			SMTP: MailSMTPConfig{
				Host:               e.str("MAIL_SMTP_HOST", ""),
				Port:               e.int("MAIL_SMTP_PORT", 587),
				Username:           e.str("MAIL_SMTP_USERNAME", ""),
				Password:           e.str("MAIL_SMTP_PASSWORD", ""),
				TLS:                strings.ToLower(strings.TrimSpace(e.str("MAIL_SMTP_TLS", mail.TLSStartTLS))),
				AllowInsecureAuth:  e.bool("MAIL_SMTP_ALLOW_INSECURE_AUTH", false),
				InsecureSkipVerify: e.bool("MAIL_SMTP_INSECURE_SKIP_VERIFY", false),
				HELO:               e.str("MAIL_SMTP_HELO", ""),
			},
			Direct: MailDirectConfig{
				HELO:               e.str("MAIL_DIRECT_HELO", ""),
				Port:               e.int("MAIL_DIRECT_PORT", 25),
				DKIMDomain:         e.str("MAIL_DKIM_DOMAIN", ""),
				DKIMSelector:       e.str("MAIL_DKIM_SELECTOR", ""),
				DKIMPrivateKey:     e.str("MAIL_DKIM_PRIVATE_KEY", ""),
				DKIMPrivateKeyFile: e.str("MAIL_DKIM_PRIVATE_KEY_FILE", ""),
			},
			Resend: MailResendConfig{
				APIKey:   e.str("MAIL_RESEND_API_KEY", ""),
				Endpoint: e.str("MAIL_RESEND_ENDPOINT", ""),
			},
		},
		SSO: SSOConfig{
			SecretKey:           e.str("SSO_SECRET_KEY", ""),
			AllowInsecureIssuer: e.bool("SSO_ALLOW_INSECURE_ISSUER", false),
			HTTPTimeout:         e.duration("SSO_HTTP_TIMEOUT", 10*time.Second),
			AuthRequestTTL:      e.duration("SSO_AUTH_REQUEST_TTL", 10*time.Minute),
			PendingTTL:          e.duration("SSO_PENDING_TTL", 5*time.Minute),
			JWKSCacheTTL:        e.duration("SSO_JWKS_CACHE_TTL", time.Hour),
			ClockSkew:           e.duration("SSO_CLOCK_SKEW", 2*time.Minute),
		},
		Inbox: InboxConfig{
			DigestQuietPeriod: e.duration("INBOX_DIGEST_QUIET_PERIOD", 10*time.Minute),
			DigestMinInterval: e.duration("INBOX_DIGEST_MIN_INTERVAL", time.Hour),
		},
		Audit: AuditConfig{
			RetentionDays:   e.int("AUDIT_RETENTION_DAYS", 365),
			Sink:            strings.ToLower(strings.TrimSpace(e.str("AUDIT_SINK", "log"))),
			SinkPath:        e.str("AUDIT_SINK_PATH", ""),
			SinkEndpoint:    e.str("AUDIT_SINK_ENDPOINT", ""),
			SinkSecret:      e.str("AUDIT_SINK_SECRET", ""),
			SinkTimeout:     e.duration("AUDIT_SINK_TIMEOUT", 10*time.Second),
			BufferSize:      e.int("AUDIT_BUFFER_SIZE", 0),
			BufferWorkers:   e.int("AUDIT_BUFFER_WORKERS", 0),
			ExportPerMinute: e.int("AUDIT_EXPORT_PER_MIN", 6),
		},
		MetricsToken: e.str("METRICS_TOKEN", ""),
		LogLevel:     e.str("LOG_LEVEL", "info"),
	}

	// Report every malformed variable at once — fixing them one restart at a
	// time is miserable, and silently falling back to a default (SERVER_PORT
	// "eighty" quietly binding 8080) is worse.
	if err := e.err(); err != nil {
		return nil, err
	}

	if err := cfg.validate(); err != nil {
		return nil, err
	}

	return cfg, nil
}

func (c *Config) validate() error {
	var errs []error

	if len(c.JWT.Secret) == 0 {
		errs = append(errs, errors.New("JWT_SECRET is required"))
	} else if len(c.JWT.Secret) < MinJWTSecretBytes {
		errs = append(errs, fmt.Errorf("JWT_SECRET must be at least %d bytes (got %d)", MinJWTSecretBytes, len(c.JWT.Secret)))
	}

	if c.Admin.Email == "" || c.Admin.Password == "" {
		errs = append(errs, errors.New("ADMIN_EMAIL and ADMIN_PASSWORD are required"))
	}

	if c.Server.Port < 1 || c.Server.Port > 65535 {
		errs = append(errs, fmt.Errorf("SERVER_PORT must be between 1 and 65535 (got %d)", c.Server.Port))
	}

	if c.DB.MaxConns < 1 {
		errs = append(errs, fmt.Errorf("DB_MAX_CONNS must be at least 1 (got %d)", c.DB.MaxConns))
	}
	if c.DB.MinConns < 0 || c.DB.MinConns > c.DB.MaxConns {
		errs = append(errs, fmt.Errorf("DB_MIN_CONNS must be between 0 and DB_MAX_CONNS (got %d)", c.DB.MinConns))
	}

	if c.RateLimit.Enabled {
		if c.RateLimit.APIPerMinute < 1 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_API_PER_MIN must be at least 1 (got %d)", c.RateLimit.APIPerMinute))
		}
		if c.RateLimit.AuthPerMinute < 1 {
			errs = append(errs, fmt.Errorf("RATE_LIMIT_AUTH_PER_MIN must be at least 1 (got %d)", c.RateLimit.AuthPerMinute))
		}
	}
	if c.RateLimit.TrustedProxyHops < 0 {
		errs = append(errs, fmt.Errorf("RATE_LIMIT_TRUSTED_PROXY_HOPS must not be negative (got %d)", c.RateLimit.TrustedProxyHops))
	}
	if c.RateLimit.TrustedProxyHops > 0 && !c.RateLimit.TrustProxy {
		errs = append(errs, errors.New("RATE_LIMIT_TRUSTED_PROXY_HOPS is set but RATE_LIMIT_TRUST_PROXY is false; X-Forwarded-For would be ignored"))
	}

	if c.Push.Enabled {
		// A mistyped endpoint would otherwise be discovered as a per-batch
		// transport failure in the worker log, long after the deploy.
		if c.Push.Endpoint != "" {
			u, err := url.Parse(c.Push.Endpoint)
			if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("EXPO_PUSH_ENDPOINT must be an absolute http(s) URL (got %q)", c.Push.Endpoint))
			}
		}
		if c.Push.Timeout <= 0 {
			errs = append(errs, fmt.Errorf("PUSH_TIMEOUT must be positive (got %s)", c.Push.Timeout))
		}
		if c.Push.QueueSize < 0 {
			errs = append(errs, fmt.Errorf("PUSH_QUEUE_SIZE must not be negative (got %d)", c.Push.QueueSize))
		}
		if c.Push.Workers < 0 {
			errs = append(errs, fmt.Errorf("PUSH_WORKERS must not be negative (got %d)", c.Push.Workers))
		}
	}

	if c.Inbox.DigestQuietPeriod <= 0 {
		errs = append(errs, fmt.Errorf("INBOX_DIGEST_QUIET_PERIOD must be positive (got %s)", c.Inbox.DigestQuietPeriod))
	}
	if c.Inbox.DigestMinInterval <= 0 {
		errs = append(errs, fmt.Errorf("INBOX_DIGEST_MIN_INTERVAL must be positive (got %s)", c.Inbox.DigestMinInterval))
	}
	if c.Audit.RetentionDays < 0 {
		errs = append(errs, fmt.Errorf("AUDIT_RETENTION_DAYS must not be negative (got %d)", c.Audit.RetentionDays))
	}
	if c.Audit.ExportPerMinute <= 0 {
		errs = append(errs, fmt.Errorf("AUDIT_EXPORT_PER_MIN must be positive (got %d)", c.Audit.ExportPerMinute))
	}
	// The sink itself is validated by audit.NewSink, which both binaries call at
	// boot: a transport named without its credentials is a boot failure, not a
	// first-use failure. Only the enum is checked here, so a typo is reported
	// alongside every other malformed variable instead of on its own.
	switch c.Audit.Sink {
	case "", "log", "file", "http":
	default:
		errs = append(errs, fmt.Errorf("AUDIT_SINK must be one of \"log\", \"file\", \"http\" (got %q)", c.Audit.Sink))
	}

	errs = append(errs, c.Mail.validate()...)
	errs = append(errs, c.validateSSO()...)

	return errors.Join(errs...)
}

// validateSSO rejects an SSO configuration that cannot work, and refuses the
// one setting that turns a tenant-supplied URL into server-side request
// forgery.
//
// The key is parsed here rather than at first use so a mistyped SSO_SECRET_KEY
// fails the boot instead of surfacing as "single sign-on is not configured" the
// first time a workspace admin saves a provider.
func (c *Config) validateSSO() []error {
	var errs []error

	if c.SSO.IsEnabled() {
		if _, err := sso.ParseSecretKey(c.SSO.SecretKey); err != nil {
			// The error already names the three accepted formats.
			errs = append(errs, err)
		}
		for _, d := range []struct {
			name string
			val  time.Duration
		}{
			{"SSO_HTTP_TIMEOUT", c.SSO.HTTPTimeout},
			{"SSO_AUTH_REQUEST_TTL", c.SSO.AuthRequestTTL},
			{"SSO_PENDING_TTL", c.SSO.PendingTTL},
			{"SSO_JWKS_CACHE_TTL", c.SSO.JWKSCacheTTL},
			{"SSO_CLOCK_SKEW", c.SSO.ClockSkew},
		} {
			if d.val <= 0 {
				errs = append(errs, fmt.Errorf("%s must be positive (got %s)", d.name, d.val))
			}
		}
	}

	// SSO_ALLOW_INSECURE_ISSUER is not a "warn and carry on" flag.
	//
	// With it on, internal/sso drops both the https requirement and the
	// dial-time private-address guard, and the issuer URL is supplied by a
	// WORKSPACE ADMIN — a tenant, not the operator of this deployment. Any one
	// of them can then point `issuer` at http://169.254.169.254/ and read the
	// cloud metadata service, i.e. the node's IAM credentials, through this
	// process. That is a full compromise of the deployment reachable from a
	// tenant-facing settings form.
	//
	// A Warn at boot is not a control: nothing reads INFO/WARN lines from a
	// pod that started successfully, and the flag's whole failure mode is
	// "someone set it once for a local Keycloak and it rode along into prod".
	// So it is fatal — except on a deployment that is self-evidently a
	// developer's, which the mail validation already has a definition for: a
	// loopback PUBLIC_BASE_URL, the value no real recipient can open. That
	// keeps the local-test-provider workflow the flag exists for and makes the
	// dangerous combination unreachable in anything that serves users.
	if c.SSO.AllowInsecureIssuer && !isLocalDeployment(c.Mail.PublicBaseURL) {
		errs = append(errs, fmt.Errorf(
			"SSO_ALLOW_INSECURE_ISSUER=true is refused on a deployment whose PUBLIC_BASE_URL is %q: "+
				"it lets any workspace admin point an OIDC issuer at a private or link-local address "+
				"(169.254.169.254) and read it through this process. "+
				"It is only accepted when PUBLIC_BASE_URL is a loopback address, i.e. a local test deployment",
			c.Mail.PublicBaseURL))
	}

	return errs
}

// isLocalDeployment reports whether the public base URL says this process is a
// developer's local stack rather than something users reach.
func isLocalDeployment(publicBaseURL string) bool {
	u, err := url.Parse(publicBaseURL)
	if err != nil || u.Host == "" {
		return false
	}
	return isLoopbackHost(u.Hostname())
}

// validate rejects a mail configuration that cannot possibly work.
//
// Every check here exists because mail misconfiguration is otherwise silent:
// nothing throws, nothing 500s, and the first symptom is a user who says they
// never received their invitation — days later, with no log line to point at.
// Selecting a transport and forgetting its one required setting must stop the
// boot.
func (c MailConfig) validate() []error {
	var errs []error

	switch c.Transport {
	case mail.TransportLog, mail.TransportSMTP, mail.TransportSMTPDirect, mail.TransportResend:
	default:
		errs = append(errs, fmt.Errorf("MAIL_TRANSPORT must be one of %q, %q, %q, %q (got %q)",
			mail.TransportLog, mail.TransportSMTP, mail.TransportSMTPDirect, mail.TransportResend, c.Transport))
		// The per-transport checks below are meaningless once the name is wrong.
		return errs
	}

	// Links are built from this on every transport, including `log` — where the
	// whole point is that a developer can paste the invitation URL out of the
	// log and it works.
	if u, err := url.Parse(c.PublicBaseURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		errs = append(errs, fmt.Errorf("PUBLIC_BASE_URL must be an absolute http(s) URL (got %q)", c.PublicBaseURL))
	} else if c.Transport != mail.TransportLog && isLoopbackHost(u.Hostname()) {
		// A real recipient cannot open http://localhost/invite/<token>. This is
		// the single most likely way to send a batch of useless invitations.
		errs = append(errs, fmt.Errorf(
			"PUBLIC_BASE_URL is %q, which no email recipient can open; set it to the address users reach this deployment on",
			c.PublicBaseURL))
	}

	if c.Timeout <= 0 {
		errs = append(errs, fmt.Errorf("MAIL_TIMEOUT must be positive (got %s)", c.Timeout))
	}
	if c.TestPerMinute < 1 {
		errs = append(errs, fmt.Errorf("MAIL_TEST_PER_MIN must be at least 1 (got %d)", c.TestPerMinute))
	}

	if c.Transport == mail.TransportLog {
		return errs
	}

	// Every sending transport needs a From: it is the envelope sender, the
	// header sender, and what SPF and DMARC are evaluated against.
	if strings.TrimSpace(c.From) == "" {
		errs = append(errs, fmt.Errorf("MAIL_FROM is required when MAIL_TRANSPORT is %q", c.Transport))
	} else if !strings.Contains(c.From, "@") {
		errs = append(errs, fmt.Errorf("MAIL_FROM must be an email address (got %q)", c.From))
	}

	switch c.Transport {
	case mail.TransportSMTP:
		if strings.TrimSpace(c.SMTP.Host) == "" {
			errs = append(errs, errors.New(`MAIL_SMTP_HOST is required when MAIL_TRANSPORT is "smtp"`))
		}
		if c.SMTP.Port < 1 || c.SMTP.Port > 65535 {
			errs = append(errs, fmt.Errorf("MAIL_SMTP_PORT must be between 1 and 65535 (got %d)", c.SMTP.Port))
		}
		switch c.SMTP.TLS {
		case mail.TLSStartTLS, mail.TLSImplicit, mail.TLSNone:
		default:
			errs = append(errs, fmt.Errorf("MAIL_SMTP_TLS must be %q, %q or %q (got %q)",
				mail.TLSStartTLS, mail.TLSImplicit, mail.TLSNone, c.SMTP.TLS))
		}
		if (c.SMTP.Username == "") != (c.SMTP.Password == "") {
			errs = append(errs, errors.New("MAIL_SMTP_USERNAME and MAIL_SMTP_PASSWORD must be set together"))
		}
		if c.SMTP.TLS == mail.TLSNone && c.SMTP.Username != "" && !c.SMTP.AllowInsecureAuth {
			errs = append(errs, errors.New(
				`MAIL_SMTP_TLS="none" with credentials set requires MAIL_SMTP_ALLOW_INSECURE_AUTH=true; SMTP AUTH sends the password in the clear`))
		}

	case mail.TransportSMTPDirect:
		if strings.TrimSpace(c.Direct.HELO) == "" {
			errs = append(errs, errors.New(
				`MAIL_DIRECT_HELO is required when MAIL_TRANSPORT is "smtp-direct": it must be this host's public FQDN, which receiving MTAs check against the connecting IP's PTR record`))
		}
		if c.Direct.Port < 1 || c.Direct.Port > 65535 {
			errs = append(errs, fmt.Errorf("MAIL_DIRECT_PORT must be between 1 and 65535 (got %d)", c.Direct.Port))
		}
		// DKIM is optional but all-or-nothing: two of the three settings is a
		// half-finished rollout that would sign with nothing.
		set := 0
		for _, v := range []string{c.Direct.DKIMDomain, c.Direct.DKIMSelector, c.Direct.dkimKeySource()} {
			if strings.TrimSpace(v) != "" {
				set++
			}
		}
		if set != 0 && set != 3 {
			errs = append(errs, errors.New(
				"MAIL_DKIM_DOMAIN, MAIL_DKIM_SELECTOR and MAIL_DKIM_PRIVATE_KEY (or MAIL_DKIM_PRIVATE_KEY_FILE) must all be set, or none of them"))
		}

	case mail.TransportResend:
		if strings.TrimSpace(c.Resend.APIKey) == "" {
			errs = append(errs, errors.New(`MAIL_RESEND_API_KEY is required when MAIL_TRANSPORT is "resend"`))
		}
		if c.Resend.Endpoint != "" {
			if u, err := url.Parse(c.Resend.Endpoint); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
				errs = append(errs, fmt.Errorf("MAIL_RESEND_ENDPOINT must be an absolute http(s) URL (got %q)", c.Resend.Endpoint))
			}
		}
	}

	return errs
}

// dkimKeySource reports whichever of the two key settings is populated.
func (c MailDirectConfig) dkimKeySource() string {
	if strings.TrimSpace(c.DKIMPrivateKeyFile) != "" {
		return c.DKIMPrivateKeyFile
	}
	return c.DKIMPrivateKey
}

// DKIMConfigured reports whether all three DKIM settings are present. Callers
// use it instead of testing the fields, so "two of three" cannot be read as
// "enabled" anywhere.
func (c MailDirectConfig) DKIMConfigured() bool {
	return strings.TrimSpace(c.DKIMDomain) != "" &&
		strings.TrimSpace(c.DKIMSelector) != "" &&
		strings.TrimSpace(c.dkimKeySource()) != ""
}

// isLoopbackHost reports whether a URL host can only be reached from the
// machine running this process.
func isLoopbackHost(host string) bool {
	switch strings.ToLower(host) {
	case "localhost", "localhost.localdomain":
		return true
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback() || ip.IsUnspecified()
	}
	return false
}

// env reads environment variables, collecting parse failures instead of
// discarding them. A malformed value is an operator mistake and must fail the
// boot, not degrade silently into a default that looks like it worked.
type env struct {
	errs []error
}

func (e *env) err() error { return errors.Join(e.errs...) }

func (e *env) fail(key, raw, kind string, err error) {
	e.errs = append(e.errs, fmt.Errorf("%s: %q is not a valid %s: %w", key, raw, kind, err))
}

func (e *env) str(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func (e *env) int(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	i, err := strconv.Atoi(v)
	if err != nil {
		e.fail(key, v, "integer", err)
		return fallback
	}
	return i
}

func (e *env) bool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.fail(key, v, "boolean", err)
		return fallback
	}
	return b
}

func (e *env) duration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.fail(key, v, "duration", err)
		return fallback
	}
	return d
}

func (e *env) list(key string, fallback []string) []string {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	parts := strings.Split(v, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	if len(out) == 0 {
		return fallback
	}
	return out
}
