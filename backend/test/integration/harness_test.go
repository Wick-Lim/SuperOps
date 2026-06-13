//go:build integration

// Package integration drives the fully-wired application (app.New) against real
// infrastructure (Postgres/Redis/NATS, plus optional MinIO/Meilisearch) over an
// httptest server. Run with:
//
//	go test -tags=integration ./test/integration/...
//
// It requires a migrated database; CI runs `cmd/migrate` first. If the infra is
// unreachable the whole suite skips rather than failing.
package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/app"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
)

type harness struct {
	app  *app.App
	srv  *httptest.Server
	base string
}

var (
	once       sync.Once
	shared     *harness
	setupErr   error
	adminEmail = "admin@company.com"
	adminPass  = "changeme_admin_password"
)

func env(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

func buildConfig() *app.Config {
	cfg := &app.Config{LogLevel: "error"}
	cfg.Server.Host = "127.0.0.1"
	cfg.Server.Port = 0
	cfg.Server.ReadTimeout = 15 * time.Second
	cfg.Server.WriteTimeout = 15 * time.Second
	cfg.Server.IdleTimeout = 60 * time.Second
	cfg.DB.Host = env("DB_HOST", "localhost")
	cfg.DB.Port = 5432
	cfg.DB.User = env("DB_USER", "superops")
	cfg.DB.Password = env("DB_PASSWORD", "changeme_db_password")
	cfg.DB.Name = env("DB_NAME", "superops")
	cfg.DB.SSLMode = env("DB_SSLMODE", "disable")
	cfg.DB.MaxConns = 10
	cfg.DB.MinConns = 2
	cfg.Redis.Addr = env("REDIS_ADDR", "localhost:6379")
	cfg.Redis.Password = env("REDIS_PASSWORD", "changeme_redis_password")
	cfg.NATS.URL = env("NATS_URL", "nats://localhost:4222")
	cfg.JWT.Secret = env("JWT_SECRET", "changeme_jwt_secret_at_least_32_chars_long")
	cfg.JWT.AccessTokenTTL = 15 * time.Minute
	cfg.JWT.RefreshTokenTTL = 720 * time.Hour
	// Optional services — left at defaults; app.New degrades gracefully if down.
	cfg.MinIO.Endpoint = env("MINIO_ENDPOINT", "localhost:19000")
	cfg.MinIO.AccessKey = env("MINIO_ACCESS_KEY", "minioadmin")
	cfg.MinIO.SecretKey = env("MINIO_SECRET_KEY", "changeme_minio_password")
	cfg.MinIO.Bucket = env("MINIO_BUCKET", "superops")
	cfg.Meili.Host = env("MEILI_HOST", "http://localhost:7700")
	cfg.Meili.MasterKey = env("MEILI_MASTER_KEY", "changeme_meili_master_key")
	cfg.Admin.Email = adminEmail
	cfg.Admin.Password = adminPass
	cfg.Admin.Username = "admin"
	cfg.RateLimit.Enabled = false // don't throttle the rapid test traffic
	cfg.CORS.AllowedOrigins = []string{"*"}
	return cfg
}

// getHarness builds the shared app+server once, skipping every test if the
// infrastructure (or schema) is not available.
func getHarness(t *testing.T) *harness {
	t.Helper()
	once.Do(func() {
		cfg := buildConfig()
		l := logger.New("error")
		ctx := context.Background()
		application, err := app.New(ctx, cfg, l)
		if err != nil {
			setupErr = fmt.Errorf("app.New: %w", err)
			return
		}
		// Verify the schema is present (migrations applied).
		if _, err := application.DB.Exec(ctx, "SELECT 1 FROM users LIMIT 1"); err != nil {
			setupErr = fmt.Errorf("schema not migrated (run cmd/migrate): %w", err)
			return
		}
		srv := httptest.NewServer(application.Server.Handler)
		shared = &harness{app: application, srv: srv, base: srv.URL}
	})
	if setupErr != nil {
		t.Skipf("integration infra unavailable: %v", setupErr)
	}
	return shared
}

// --- HTTP helpers ---

type apiResp struct {
	Data  json.RawMessage `json:"data"`
	Meta  *struct {
		Cursor  string `json:"cursor"`
		HasMore bool   `json:"has_more"`
	} `json:"meta"`
	Error *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func (h *harness) do(t *testing.T, method, path, token string, body any) (int, apiResp) {
	t.Helper()
	var rdr io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequest(method, h.base+path, rdr)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer res.Body.Close()
	raw, _ := io.ReadAll(res.Body)
	var ar apiResp
	if len(raw) > 0 && (raw[0] == '{' || raw[0] == '[') {
		_ = json.Unmarshal(raw, &ar)
	}
	return res.StatusCode, ar
}

func (h *harness) login(t *testing.T, email, pass string) string {
	t.Helper()
	code, r := h.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": email, "password": pass})
	if code != 200 {
		t.Fatalf("login %s: status %d (%v)", email, code, r.Error)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(r.Data, &tok); err != nil || tok.AccessToken == "" {
		t.Fatalf("login %s: no access token (%v)", email, err)
	}
	return tok.AccessToken
}

func (h *harness) adminToken(t *testing.T) string { return h.login(t, adminEmail, adminPass) }

func (h *harness) firstWorkspace(t *testing.T, token string) string {
	t.Helper()
	_, r := h.do(t, "GET", "/api/v1/workspaces", token, nil)
	var ws []struct {
		ID string `json:"id"`
	}
	json.Unmarshal(r.Data, &ws)
	if len(ws) == 0 {
		t.Fatal("no workspace for admin")
	}
	return ws[0].ID
}

// createChannel makes a fresh public channel and returns its id.
func (h *harness) createChannel(t *testing.T, token, wsID, slug string) string {
	t.Helper()
	code, r := h.do(t, "POST", "/api/v1/workspaces/"+wsID+"/channels", token,
		map[string]string{"name": slug, "slug": slug, "type": "public"})
	if code != 201 {
		t.Fatalf("create channel: status %d (%v)", code, r.Error)
	}
	var ch struct {
		ID string `json:"id"`
	}
	json.Unmarshal(r.Data, &ch)
	if ch.ID == "" {
		t.Fatal("create channel: empty id")
	}
	return ch.ID
}

func uniqueSlug(prefix string) string {
	return fmt.Sprintf("%s-%d", prefix, time.Now().UnixNano())
}

// wsScheme converts the httptest http URL to a ws URL for the WS endpoint.
func (h *harness) wsURL(token string) string {
	u := strings.Replace(h.base, "http://", "ws://", 1)
	return u + "/api/v1/ws?token=" + token
}
