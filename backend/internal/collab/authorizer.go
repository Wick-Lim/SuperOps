package collab

import (
	"context"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
)

// Authorizer answers who may open, edit and create collaborative documents.
//
// It exists as an interface for one reason: the object-level permission model
// in ROADMAP §4 replaces the workspace-shaped answer below with a (subject,
// object, capability) one, and when it does this is the only place that
// changes. It is not a wrapper around internal/authz — the implementation calls
// straight through to it, and no membership SQL is written anywhere in this
// package.
//
// Every method returns (bool, error) with the usual rule: err is a failure to
// decide (500), !ok is a denial (403). Collapsing them turns a database blip
// into an authorization bypass in one direction and a lockout in the other.
type Authorizer interface {
	CanRead(ctx context.Context, doc *Document, userID string) (bool, error)
	CanWrite(ctx context.Context, doc *Document, userID string) (bool, error)
	CanCreate(ctx context.Context, workspaceID, userID string) (bool, error)
}

// WorkspaceAuthorizer is the Phase-0 implementation: a document is readable by
// any member of its workspace and writable by anyone above guest.
//
// It now asks the object-level checker for a capability on the workspace rather
// than for a role, which is why the ladder is ordered: guest maps to CapRead and
// member to CapWrite, so "readable by members, writable by anyone above guest"
// is two capability comparisons instead of a role switch. Guests stay read-only
// because that is what the role means everywhere else in the product, and
// because a guest who can write is a guest who can rewrite a document wholesale
// via a snapshot.
//
// The workspace is still the object being authorized. Documents get acl_object
// rows of their own — and CanRead becomes a Can against the DOCUMENT, sharing
// and all — when Drive lands the folder hierarchy they hang off; until then a
// document has no place in the tree to inherit from, and inventing one here
// would be an ACL the drift verifier could not reconcile.
type WorkspaceAuthorizer struct {
	checker *authz.Checker
}

func NewWorkspaceAuthorizer(checker *authz.Checker) *WorkspaceAuthorizer {
	return &WorkspaceAuthorizer{checker: checker}
}

func (a *WorkspaceAuthorizer) CanRead(ctx context.Context, doc *Document, userID string) (bool, error) {
	if doc == nil {
		return false, nil
	}
	return a.checker.Can(ctx, authz.UserSubject(userID), authz.WorkspaceObject(doc.WorkspaceID), authz.CapRead)
}

func (a *WorkspaceAuthorizer) CanWrite(ctx context.Context, doc *Document, userID string) (bool, error) {
	if doc == nil {
		return false, nil
	}
	return a.canEdit(ctx, doc.WorkspaceID, userID)
}

func (a *WorkspaceAuthorizer) CanCreate(ctx context.Context, workspaceID, userID string) (bool, error) {
	return a.canEdit(ctx, workspaceID, userID)
}

func (a *WorkspaceAuthorizer) canEdit(ctx context.Context, workspaceID, userID string) (bool, error) {
	return a.checker.Can(ctx, authz.UserSubject(userID), authz.WorkspaceObject(workspaceID), authz.CapWrite)
}
