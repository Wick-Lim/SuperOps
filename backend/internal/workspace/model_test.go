package workspace

import (
	"net/http"
	"testing"
)

func TestValidateRoleChange(t *testing.T) {
	tests := []struct {
		name       string
		actorRole  string
		targetRole string
		newRole    string
		wantStatus int // 0 means "allowed"
		wantCode   string
	}{
		{
			name:      "owner demotes an admin",
			actorRole: RoleOwner, targetRole: RoleAdmin, newRole: RoleMember,
		},
		{
			name:      "admin promotes a member",
			actorRole: RoleAdmin, targetRole: RoleMember, newRole: RoleAdmin,
		},
		{
			name:      "admin converts a guest",
			actorRole: RoleAdmin, targetRole: RoleGuest, newRole: RoleMember,
		},
		{
			name:      "unknown role is rejected before it reaches the CHECK constraint",
			actorRole: RoleOwner, targetRole: RoleMember, newRole: "superuser",
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ROLE",
		},
		{
			name:      "empty role is rejected",
			actorRole: RoleOwner, targetRole: RoleMember, newRole: "",
			wantStatus: http.StatusBadRequest, wantCode: "INVALID_ROLE",
		},
		{
			name:      "plain member cannot change roles",
			actorRole: RoleMember, targetRole: RoleMember, newRole: RoleAdmin,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name:      "non-member cannot change roles",
			actorRole: "", targetRole: RoleMember, newRole: RoleAdmin,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name:      "admin cannot promote themselves to owner",
			actorRole: RoleAdmin, targetRole: RoleAdmin, newRole: RoleOwner,
			wantStatus: http.StatusBadRequest, wantCode: "USE_OWNERSHIP_TRANSFER",
		},
		{
			name:      "even the owner grants ownership only through the transfer path",
			actorRole: RoleOwner, targetRole: RoleMember, newRole: RoleOwner,
			wantStatus: http.StatusBadRequest, wantCode: "USE_OWNERSHIP_TRANSFER",
		},
		{
			name:      "admin cannot demote the owner",
			actorRole: RoleAdmin, targetRole: RoleOwner, newRole: RoleMember,
			wantStatus: http.StatusForbidden, wantCode: "OWNER_PROTECTED",
		},
		{
			name:      "owner cannot demote themselves and leave the workspace ownerless",
			actorRole: RoleOwner, targetRole: RoleOwner, newRole: RoleAdmin,
			wantStatus: http.StatusForbidden, wantCode: "OWNER_PROTECTED",
		},
		{
			name:      "updating a non-member is a 404, not a silent 200",
			actorRole: RoleAdmin, targetRole: "", newRole: RoleMember,
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateRoleChange(tt.actorRole, tt.targetRole, tt.newRole)
			if tt.wantStatus == 0 {
				if got != nil {
					t.Fatalf("validateRoleChange(%q, %q, %q) = %+v, want allowed", tt.actorRole, tt.targetRole, tt.newRole, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("validateRoleChange(%q, %q, %q) = allowed, want %d %s", tt.actorRole, tt.targetRole, tt.newRole, tt.wantStatus, tt.wantCode)
			}
			if got.Status != tt.wantStatus || got.Code != tt.wantCode {
				t.Errorf("validateRoleChange(%q, %q, %q) = %d %s, want %d %s",
					tt.actorRole, tt.targetRole, tt.newRole, got.Status, got.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestValidateRemoval(t *testing.T) {
	tests := []struct {
		name       string
		actorRole  string
		targetRole string
		wantStatus int // 0 means "allowed"
		wantCode   string
	}{
		{name: "owner removes an admin", actorRole: RoleOwner, targetRole: RoleAdmin},
		{name: "admin removes a member", actorRole: RoleAdmin, targetRole: RoleMember},
		{name: "admin removes a guest", actorRole: RoleAdmin, targetRole: RoleGuest},
		{
			name: "member cannot remove anyone", actorRole: RoleMember, targetRole: RoleMember,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name: "non-member cannot remove anyone", actorRole: "", targetRole: RoleMember,
			wantStatus: http.StatusForbidden, wantCode: "FORBIDDEN",
		},
		{
			name: "admin cannot evict the owner", actorRole: RoleAdmin, targetRole: RoleOwner,
			wantStatus: http.StatusForbidden, wantCode: "OWNER_PROTECTED",
		},
		{
			name: "owner cannot remove themselves", actorRole: RoleOwner, targetRole: RoleOwner,
			wantStatus: http.StatusForbidden, wantCode: "OWNER_PROTECTED",
		},
		{
			name: "removing a non-member is a 404", actorRole: RoleAdmin, targetRole: "",
			wantStatus: http.StatusNotFound, wantCode: "NOT_FOUND",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := validateRemoval(tt.actorRole, tt.targetRole)
			if tt.wantStatus == 0 {
				if got != nil {
					t.Fatalf("validateRemoval(%q, %q) = %+v, want allowed", tt.actorRole, tt.targetRole, got)
				}
				return
			}
			if got == nil {
				t.Fatalf("validateRemoval(%q, %q) = allowed, want %d %s", tt.actorRole, tt.targetRole, tt.wantStatus, tt.wantCode)
			}
			if got.Status != tt.wantStatus || got.Code != tt.wantCode {
				t.Errorf("validateRemoval(%q, %q) = %d %s, want %d %s",
					tt.actorRole, tt.targetRole, got.Status, got.Code, tt.wantStatus, tt.wantCode)
			}
		})
	}
}

func TestValidRole(t *testing.T) {
	for _, role := range []string{RoleOwner, RoleAdmin, RoleMember, RoleGuest} {
		if !validRole(role) {
			t.Errorf("validRole(%q) = false, want true", role)
		}
	}
	for _, role := range []string{"", "Owner", "root", "admin "} {
		if validRole(role) {
			t.Errorf("validRole(%q) = true, want false", role)
		}
	}
}
