package rbac

import (
	"net/http"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

func RequireWorkspaceRole(pool *pgxpool.Pool, roles ...string) func(http.Handler) http.Handler {
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := authctx.UserID(r.Context())
			workspaceID := r.PathValue("workspace_id")
			if workspaceID == "" {
				workspaceID = authctx.WorkspaceID(r.Context())
			}

			if userID == "" || workspaceID == "" {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "access denied")
				return
			}

			var role string
			err := pool.QueryRow(r.Context(),
				`SELECT role FROM workspace_members WHERE workspace_id = $1 AND user_id = $2`,
				workspaceID, userID,
			).Scan(&role)
			if err != nil || !roleSet[role] {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
