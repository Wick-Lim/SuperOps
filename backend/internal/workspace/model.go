package workspace

import (
	"net/http"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// Workspace roles, mirroring the CHECK constraint on workspace_members.role.
// The values match internal/authz's vocabulary; they are duplicated here only
// so request validation does not depend on the authorization layer.
const (
	RoleOwner  = "owner"
	RoleAdmin  = "admin"
	RoleMember = "member"
	RoleGuest  = "guest"
)

// validRole reports whether role is one of the four the CHECK constraint
// accepts. Without this an arbitrary string reached Postgres and a constraint
// violation surfaced as a 500 instead of a 400.
func validRole(role string) bool {
	switch role {
	case RoleOwner, RoleAdmin, RoleMember, RoleGuest:
		return true
	}
	return false
}

// validateRoleChange decides whether an actor holding actorRole may change a
// member holding targetRole to newRole. It returns nil when the change is
// allowed, and the error to answer with otherwise.
//
// Two things it deliberately forbids, both of which used to be possible for any
// admin: granting 'owner' (an admin could promote themselves and take over the
// workspace) and moving the current owner off 'owner' (which left the workspace
// with an owner_id pointing at a non-owner, and nobody able to delete it).
// Ownership moves only through TransferOwnership, which reassigns both sides
// atomically.
func validateRoleChange(actorRole, targetRole, newRole string) *httputil.AppError {
	if !validRole(newRole) {
		return &httputil.AppError{Status: http.StatusBadRequest, Code: "INVALID_ROLE", Message: "role must be one of 'owner', 'admin', 'member', 'guest'"}
	}
	if actorRole != RoleOwner && actorRole != RoleAdmin {
		return httputil.NewForbidden("insufficient permissions")
	}
	if newRole == RoleOwner {
		return &httputil.AppError{Status: http.StatusBadRequest, Code: "USE_OWNERSHIP_TRANSFER", Message: "use the ownership transfer endpoint to grant 'owner'"}
	}
	if targetRole == "" {
		return httputil.NewNotFound("member not found")
	}
	if targetRole == RoleOwner {
		return &httputil.AppError{Status: http.StatusForbidden, Code: "OWNER_PROTECTED", Message: "the workspace owner's role cannot be changed; transfer ownership first"}
	}
	return nil
}

// validateRemoval decides whether an actor holding actorRole may evict a member
// holding targetRole. Admins used to be able to evict the owner.
func validateRemoval(actorRole, targetRole string) *httputil.AppError {
	if actorRole != RoleOwner && actorRole != RoleAdmin {
		return httputil.NewForbidden("insufficient permissions")
	}
	if targetRole == "" {
		return httputil.NewNotFound("member not found")
	}
	if targetRole == RoleOwner {
		return &httputil.AppError{Status: http.StatusForbidden, Code: "OWNER_PROTECTED", Message: "the workspace owner cannot be removed; transfer ownership first"}
	}
	return nil
}

type Workspace struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	Slug        string    `json:"slug"`
	Description string    `json:"description"`
	IconURL     string    `json:"icon_url"`
	OwnerID     string    `json:"owner_id"`
	CreatedAt   time.Time `json:"created_at"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type Member struct {
	WorkspaceID string    `json:"workspace_id"`
	UserID      string    `json:"user_id"`
	Role        string    `json:"role"`
	JoinedAt    time.Time `json:"joined_at"`
}
