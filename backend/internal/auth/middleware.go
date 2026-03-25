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

func extractToken(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	if strings.HasPrefix(auth, "Bearer ") {
		return strings.TrimPrefix(auth, "Bearer ")
	}

	if token := r.URL.Query().Get("token"); token != "" {
		return token
	}

	return ""
}
