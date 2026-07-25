package auth

import (
	"net/http"
	"strings"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

func Middleware(jwtMgr *JWTManager) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			tokenStr := extractToken(r)
			if tokenStr == "" {
				httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "missing authorization token")
				return
			}

			claims, err := jwtMgr.Validate(tokenStr)
			if err != nil {
				httputil.JSONError(w, http.StatusUnauthorized, "UNAUTHORIZED", "invalid or expired token")
				return
			}

			ctx := r.Context()
			ctx = authctx.WithUserID(ctx, claims.UserID)
			if claims.WorkspaceID != "" {
				ctx = authctx.WithWorkspaceID(ctx, claims.WorkspaceID)
			}
			if claims.Role != "" {
				ctx = authctx.WithRole(ctx, claims.Role)
			}

			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// bearerToken returns the token from the Authorization header, or "".
func bearerToken(r *http.Request) string {
	const prefix = "Bearer "
	auth := r.Header.Get("Authorization")
	if len(auth) > len(prefix) && strings.EqualFold(auth[:len(prefix)], prefix) {
		return auth[len(prefix):]
	}
	return ""
}

// allowsQueryToken reports whether a request may carry its access token in the
// `?token=` query parameter.
//
// A token in a URL leaks into proxy access logs, browser history and the
// Referer header of anything the response links to, so it is restricted to the
// two endpoints whose clients genuinely cannot set a request header: the
// WebSocket handshake (the browser WebSocket API has no header hook) and file
// download (used directly as an <img>/<video> src). Everything else must use
// the Authorization header.
func allowsQueryToken(r *http.Request) bool {
	path := strings.TrimSuffix(r.URL.Path, "/")
	switch {
	case r.Method == http.MethodGet && path == "/api/v1/ws":
		return true
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/v1/files/"):
		// Exactly /api/v1/files/{file_id} — no deeper sub-resource.
		return !strings.Contains(strings.TrimPrefix(path, "/api/v1/files/"), "/")
	default:
		return false
	}
}

func extractToken(r *http.Request) string {
	if token := bearerToken(r); token != "" {
		return token
	}

	if allowsQueryToken(r) {
		return r.URL.Query().Get("token")
	}

	return ""
}
