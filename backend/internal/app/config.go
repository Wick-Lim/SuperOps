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
	MetricsToken string // METRICS_TOKEN — if set, GET /metrics requires this bearer token
	LogLevel     string
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

	return errors.Join(errs...)
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
