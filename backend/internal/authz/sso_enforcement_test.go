package authz

import (
	"context"
	"testing"
)

// PasswordLoginAllowed is the whole of "this organization requires SSO", and
// the part that is easy to get wrong is not the enforced case — it is the
// interaction of a global session with per-workspace enforcement, and the owner
// exemption that keeps an administrator from locking themselves out.
//
// The fixtures are built here rather than in the shared world so that turning
// enforcement on cannot affect any other test in the package.
func TestPasswordLoginAllowed(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	c := New(pool)

	b := &builder{ctx: ctx, pool: pool}

	owner := b.user("sso-owner")
	admin := b.user("sso-admin")
	member := b.user("sso-member")
	outsider := b.user("sso-outsider")
	// bothWorkspaces belongs to an enforced workspace and an unenforced one:
	// a session is global, so one enforced membership is enough to block them.
	bothWorkspaces := b.user("sso-both")
	ownerElsewhere := b.user("sso-owner-elsewhere")

	enforced := b.workspace("sso-enforced", owner)
	b.member_(enforced, owner, RoleOwner)
	b.member_(enforced, admin, RoleAdmin)
	b.member_(enforced, member, RoleMember)
	b.member_(enforced, bothWorkspaces, RoleMember)

	relaxed := b.workspace("sso-relaxed", ownerElsewhere)
	b.member_(relaxed, ownerElsewhere, RoleOwner)
	b.member_(relaxed, bothWorkspaces, RoleMember)
	b.member_(relaxed, outsider, RoleMember)

	if b.err != nil {
		t.Fatalf("seed: %v", b.err)
	}

	var providerID string
	if err := pool.QueryRow(ctx,
		`INSERT INTO sso_providers (workspace_id, issuer, client_id, redirect_uri, enabled, enforced)
		 VALUES ($1, 'https://idp.example.com', 'client', 'https://app.example.com/cb', TRUE, TRUE)
		 RETURNING id`, enforced).Scan(&providerID); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	tests := []struct {
		name   string
		userID string
		want   bool
	}{
		{"member of the enforced workspace", member, false},
		{"admin of the enforced workspace", admin, false},
		{"owner keeps the break-glass", owner, true},
		{"member of an unenforced workspace", outsider, true},
		{"one enforced membership blocks a global session", bothWorkspaces, false},
		{"owner of an unrelated workspace", ownerElsewhere, true},
		{"empty user id", "", false},
		{"unknown user", missingID, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.PasswordLoginAllowed(ctx, tt.userID)
			if err != nil {
				t.Fatalf("PasswordLoginAllowed: %v", err)
			}
			if got != tt.want {
				t.Errorf("PasswordLoginAllowed = %v, want %v", got, tt.want)
			}
		})
	}

	// Giving up the exemption removes the owner's password login too.
	if _, err := pool.Exec(ctx,
		`UPDATE sso_providers SET allow_owner_password_login = FALSE WHERE id = $1`, providerID); err != nil {
		t.Fatalf("disable exemption: %v", err)
	}
	if allowed, err := c.PasswordLoginAllowed(ctx, owner); err != nil || allowed {
		t.Errorf("owner allowed = %v (err %v), want false once the exemption is off", allowed, err)
	}

	// A provider that is configured but switched off enforces nothing.
	if _, err := pool.Exec(ctx,
		`UPDATE sso_providers SET enforced = FALSE, enabled = FALSE WHERE id = $1`, providerID); err != nil {
		t.Fatalf("disable provider: %v", err)
	}
	if allowed, err := c.PasswordLoginAllowed(ctx, member); err != nil || !allowed {
		t.Errorf("member allowed = %v (err %v), want true with the provider disabled", allowed, err)
	}
}

func TestSSOEnforced(t *testing.T) {
	pool := testDB(t)
	ctx := context.Background()
	c := New(pool)

	b := &builder{ctx: ctx, pool: pool}
	owner := b.user("enforced-check-owner")
	enforced := b.workspace("enforced-check", owner)
	plain := b.workspace("unenforced-check", owner)
	b.member_(enforced, owner, RoleOwner)
	b.member_(plain, owner, RoleOwner)
	if b.err != nil {
		t.Fatalf("seed: %v", b.err)
	}

	if _, err := pool.Exec(ctx,
		`INSERT INTO sso_providers (workspace_id, issuer, client_id, redirect_uri, enabled, enforced)
		 VALUES ($1, 'https://idp.example.com', 'client', 'https://app.example.com/cb', TRUE, TRUE)`,
		enforced); err != nil {
		t.Fatalf("create provider: %v", err)
	}

	tests := []struct {
		name        string
		workspaceID string
		want        bool
	}{
		{"enforced", enforced, true},
		{"no provider at all", plain, false},
		{"empty id", "", false},
		{"unknown workspace", missingID, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.SSOEnforced(ctx, tt.workspaceID)
			if err != nil {
				t.Fatalf("SSOEnforced: %v", err)
			}
			if got != tt.want {
				t.Errorf("SSOEnforced = %v, want %v", got, tt.want)
			}
		})
	}
}
