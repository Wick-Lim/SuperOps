package authctx

import (
	"context"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		put  func(context.Context, string) context.Context
		get  func(context.Context) string
	}{
		{"user id", WithUserID, UserID},
		{"workspace id", WithWorkspaceID, WorkspaceID},
		{"role", WithRole, Role},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ctx := tt.put(context.Background(), "value")
			if got := tt.get(ctx); got != "value" {
				t.Errorf("got %q, want %q", got, "value")
			}
			// The last write wins, which is what makes a re-authenticating
			// middleware safe to run twice.
			ctx = tt.put(ctx, "second")
			if got := tt.get(ctx); got != "second" {
				t.Errorf("after overwrite got %q, want %q", got, "second")
			}
		})
	}
}

// TestMissingValueIsEmpty is the property every caller depends on: an
// unauthenticated context must read as "" rather than panic, because every
// authorization check in the codebase starts with authctx.UserID(ctx) and
// compares it against "".
func TestMissingValueIsEmpty(t *testing.T) {
	ctx := context.Background()

	if got := UserID(ctx); got != "" {
		t.Errorf("UserID on a bare context = %q, want empty", got)
	}
	if got := WorkspaceID(ctx); got != "" {
		t.Errorf("WorkspaceID on a bare context = %q, want empty", got)
	}
	if got := Role(ctx); got != "" {
		t.Errorf("Role on a bare context = %q, want empty", got)
	}
}

// TestValuesDoNotCollide pins the three keys to three separate slots. They are
// distinct constants of the same unexported type, so a copy-paste that reused
// one key would silently make WorkspaceID answer with the user id — and every
// workspace-scoped query would then be keyed off an id that is never a
// workspace.
func TestValuesDoNotCollide(t *testing.T) {
	ctx := WithUserID(context.Background(), "user")
	ctx = WithWorkspaceID(ctx, "workspace")
	ctx = WithRole(ctx, "owner")

	if got := UserID(ctx); got != "user" {
		t.Errorf("UserID = %q, want %q", got, "user")
	}
	if got := WorkspaceID(ctx); got != "workspace" {
		t.Errorf("WorkspaceID = %q, want %q", got, "workspace")
	}
	if got := Role(ctx); got != "owner" {
		t.Errorf("Role = %q, want %q", got, "owner")
	}
}

// TestKeyTypeIsPrivate proves the ctxKey wrapper is doing its job: a value
// stored by any other package under the plain string "user_id" must be
// invisible here. Without the distinct key type, any middleware — including one
// from a third-party dependency — could inject an identity.
func TestKeyTypeIsPrivate(t *testing.T) {
	//nolint:staticcheck // using a plain string key is the point of this test.
	ctx := context.WithValue(context.Background(), "user_id", "spoofed")
	if got := UserID(ctx); got != "" {
		t.Errorf("UserID picked up a plain-string key: %q", got)
	}
}

// TestWrongTypeIsEmpty covers the type assertion. A non-string stored under the
// real key must read as "" rather than panic.
func TestWrongTypeIsEmpty(t *testing.T) {
	ctx := context.WithValue(context.Background(), keyUserID, 42)
	if got := UserID(ctx); got != "" {
		t.Errorf("UserID on a non-string value = %q, want empty", got)
	}
}

// TestParentIsNotMutated: middleware derives a child context and passes it on,
// so writing to the child must leave the parent (and any sibling request) alone.
func TestParentIsNotMutated(t *testing.T) {
	parent := WithUserID(context.Background(), "alice")
	child := WithUserID(parent, "bob")

	if got := UserID(parent); got != "alice" {
		t.Errorf("parent UserID = %q, want alice", got)
	}
	if got := UserID(child); got != "bob" {
		t.Errorf("child UserID = %q, want bob", got)
	}
}
