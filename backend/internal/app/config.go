package app

import (
	"fmt"
	"os"
	"strconv"
	"time"
)

type Config struct {
	Server   ServerConfig
	DB       DBConfig
	Redis    RedisConfig
	NATS     NATSConfig
	JWT      JWTConfig
	MinIO    MinIOConfig
	Meili    MeiliConfig
	LogLevel string
}

type ServerConfig struct {
	Host         string
	Port         int
	ReadTimeout  time.Duration
	WriteTimeout time.Duration
	IdleTimeout  time.Duration
}

type DBConfig struct {
	Host     string
	Port     int
	User     string
	Password string
	Name     string
	SSLMode  string
	MaxConns int32
	MinConns int32
}

func (c DBConfig) DSN() string {
	return fmt.Sprintf("postgres://%s:%s@%s:%d/%s?sslmode=%s",
		c.User, c.Password, c.Host, c.Port, c.Name, c.SSLMode)
}

type RedisConfig struct {
	Addr     string
	Password string
	DB       int
}

type NATSConfig struct {
	URL string
}

type JWTConfig struct {
	Secret          string
	AccessTokenTTL  time.Duration
	RefreshTokenTTL time.Duration
}

type MinIOConfig struct {
	Endpoint  string
	AccessKey string
	SecretKey string
	Bucket    string
	UseSSL    bool
}

type MeiliConfig struct {
	Host      string
	MasterKey string
}

func LoadConfig() (*Config, error) {
	cfg := &Config{
		Server: ServerConfig{
			Host:         envStr("SERVER_HOST", "0.0.0.0"),
			Port:         envInt("SERVER_PORT", 8080),
			ReadTimeout:  envDuration("SERVER_READ_TIMEOUT", 15*time.Second),
			WriteTimeout: envDuration("SERVER_WRITE_TIMEOUT", 15*time.Second),
			IdleTimeout:  envDuration("SERVER_IDLE_TIMEOUT", 60*time.Second),
		},
		DB: DBConfig{
			Host:     envStr("DB_HOST", "localhost"),
			Port:     envInt("DB_PORT", 5432),
			User:     envStr("DB_USER", "superops"),
			Password: envStr("DB_PASSWORD", "superops"),
			Name:     envStr("DB_NAME", "superops"),
			SSLMode:  envStr("DB_SSLMODE", "disable"),
			MaxConns: int32(envInt("DB_MAX_CONNS", 25)),
			MinConns: int32(envInt("DB_MIN_CONNS", 5)),
		},
		Redis: RedisConfig{
			Addr:     envStr("REDIS_ADDR", "localhost:6379"),
			Password: envStr("REDIS_PASSWORD", ""),
			DB:       envInt("REDIS_DB", 0),
		},
		NATS: NATSConfig{
			URL: envStr("NATS_URL", "nats://localhost:4222"),
		},
		JWT: JWTConfig{
			Secret:          envStr("JWT_SECRET", ""),
			AccessTokenTTL:  envDuration("JWT_ACCESS_TTL", 15*time.Minute),
			RefreshTokenTTL: envDuration("JWT_REFRESH_TTL", 30*24*time.Hour),
		},
		MinIO: MinIOConfig{
			Endpoint:  envStr("MINIO_ENDPOINT", "localhost:9000"),
			AccessKey: envStr("MINIO_ACCESS_KEY", "minioadmin"),
			SecretKey: envStr("MINIO_SECRET_KEY", "minioadmin"),
			Bucket:    envStr("MINIO_BUCKET", "superops"),
			UseSSL:    envBool("MINIO_USE_SSL", false),
		},
		Meili: MeiliConfig{
			Host:      envStr("MEILI_HOST", "http://localhost:7700"),
			MasterKey: envStr("MEILI_MASTER_KEY", ""),
		},
		LogLevel: envStr("LOG_LEVEL", "info"),
	}

	if cfg.JWT.Secret == "" {
		return nil, fmt.Errorf("JWT_SECRET environment variable is required")
	}

	return cfg, nil
}

func envStr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	if v := os.Getenv(key); v != "" {
		if i, err := strconv.Atoi(v); err == nil {
			return i
		}
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	if v := os.Getenv(key); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return fallback
}

func envDuration(key string, fallback time.Duration) time.Duration {
	if v := os.Getenv(key); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return fallback
}
