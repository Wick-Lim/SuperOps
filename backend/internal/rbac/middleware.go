// Package rbac provides route-level role gates on top of internal/authz.
//
// These are coarse gates only. A middleware can decide "this caller is allowed
// into this route group"; it cannot decide "this caller may act on this row",
// because the row is not known until the handler resolves it. Every handler
// behind these gates is still responsible for scoping its own queries — see
// the package doc on internal/authz.
package rbac

import (
	"net/http"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// RequireWorkspaceRole admits a caller holding any of the given roles in the
// workspace named by the {workspace_id} path parameter.
//
// The parameter is mandatory. The previous version fell back to
// authctx.WorkspaceID when the path had none — a value that is always empty in
// production, so the gate silently denied every such route and the middleware
// was never wired anywhere. Requiring the path param makes the gate honest:
// use it only on workspace-scoped routes.
func RequireWorkspaceRole(az *authz.Checker, roles ...string) func(http.Handler) http.Handler {
	roleSet := make(map[string]bool, len(roles))
	for _, r := range roles {
		roleSet[r] = true
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := authctx.UserID(r.Context())
			workspaceID := r.PathValue("workspace_id")
			if userID == "" || workspaceID == "" {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "access denied")
				return
			}

			role, err := az.WorkspaceRole(r.Context(), workspaceID, userID)
			if err != nil {
				// A database outage is not an authorization decision. Collapsing
				// it into 403 would make an outage look like a permissions bug
				// and hide it from error-rate alerting.
				httputil.HandleError(w, httputil.NewInternal(err))
				return
			}
			if !roleSet[role] {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "insufficient permissions")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}

// RequireAnyWorkspaceAdmin admits a caller who administers at least one
// workspace. It is the door to the /api/v1/admin/* route group and nothing
// more.
//
// It replaces RequireSystemAdmin, which asked exactly this question but was
// named — and used — as though it proved instance-wide authority. It did not:
// anyone who creates a workspace is its owner, so that gate handed every
// caller the whole admin surface, including listing and mutating users in
// other tenants. The handlers behind this gate must scope every query to
// authz.AdminWorkspaceIDs / authz.SharesWorkspace; this middleware only spares
// them from running that check for callers who administer nothing at all.
func RequireAnyWorkspaceAdmin(az *authz.Checker) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			userID := authctx.UserID(r.Context())
			if userID == "" {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "access denied")
				return
			}

			ids, err := az.AdminWorkspaceIDs(r.Context(), userID)
			if err != nil {
				httputil.HandleError(w, httputil.NewInternal(err))
				return
			}
			if len(ids) == 0 {
				httputil.JSONError(w, http.StatusForbidden, "FORBIDDEN", "admin privileges required")
				return
			}

			next.ServeHTTP(w, r)
		})
	}
}
