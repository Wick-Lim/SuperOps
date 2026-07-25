package authz

// The object-level permission checker — docs/plans/00-permissions.md.
//
// This file is the general case: subject, object, ordered capability, explicit
// grant, materialized path. Every authorization decision in the product goes
// through Can — the membership methods it replaced were deleted in step 5 of
// the plan's migration section, once nothing called them and the suite was
// green with the dual-run comparison on.
//
// Three invariants are worth stating before the code:
//
//   - The pre-ACL tables stay AUTHORITATIVE for workspaces, channels and files.
//     Capability computes their layer from workspace_members / channel_members /
//     channels.type / files.message_id directly, NOT from acl_object — so it is
//     correct for a channel created one second ago, and so a missing
//     materialization row can never widen an answer. acl_object and acl_key hold
//     the same answer, materialized, for the list path; the worker's job
//     reconciles and then verifies them.
//
//   - N+1 is the failure mode this design exists to prevent. Can answers about
//     ONE object and costs two to three queries; a list endpoint that calls it
//     per row is wrong. The list path is KeysFor once per request followed by
//     FilterReadable (or a `key = ANY($keys)` join) over the whole page.
//
//   - A revocation is not finished when the row is deleted. Grants are what
//     handlers enforce now, so Revoke and Move drive the Revoker over every
//     object in the subtree that somebody may hold an open connection to.

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/pkg/database"
)

// ErrNotRegistered is returned by the mutations when an object has no
// acl_object row. It is a distinct error because the caller's remedy is
// specific: register the object (or, for a derived type, run Rebuild), not
// retry.
var ErrNotRegistered = errors.New("object is not registered in acl_object")

// ErrDerivedType is returned when a mutation targets an object whose hierarchy
// is computed from the pre-ACL schema. Registering or moving one would be
// reverted by the next Rebuild and reported as drift in the meantime.
var ErrDerivedType = errors.New("object type's hierarchy is derived from the legacy schema")

// ErrMoveCycle and ErrMoveTooDeep are Move's two structural refusals. They are
// sentinels rather than message text because a caller has to turn them into an
// HTTP status, and matching on a sentence is a translation that stops working
// silently the day somebody rewords it.
var (
	ErrMoveCycle   = errors.New("the destination is inside the object being moved")
	ErrMoveTooDeep = errors.New("the subtree would exceed the maximum path depth")

	// ErrCrossWorkspaceMove is a move between tenants. Never silently allowed:
	// acl_object.workspace_id is authoritative for tenancy.
	ErrCrossWorkspaceMove = errors.New("objects cannot move between workspaces")
)

// ---------------------------------------------------------------------------
// Resolution
// ---------------------------------------------------------------------------

// objectState is everything a capability decision needs about one object,
// gathered in a single query.
type objectState struct {
	ref         ObjectRef
	workspaceID string
	path        string

	// Legacy extras, set only for derived types.
	channelType   string // for a channel: 'public' | 'private' | 'dm'
	parentChannel string // for a file: the channel of the message it hangs off
	ownerID       string // for a file: files.user_id
}

// resolve loads an object. It returns (nil, nil) when the object does not
// exist, matching the repository convention used throughout the codebase — and
// mattering more here, because "does not exist" must reach the handler as a 404
// and never as a 500.
func (c *Checker) resolve(ctx context.Context, obj ObjectRef) (*objectState, error) {
	st := &objectState{ref: obj}
	switch obj.Type {
	case TypeWorkspace:
		// No existence probe. A workspace is its own root, so its id, its
		// workspace id and its path are the same fact and none of them needs a
		// query — and the two things that consume this state already answer
		// "no" for a workspace that does not exist: WorkspaceRole finds no
		// membership row, and acl_grant has a foreign key to acl_object, which
		// has one to workspaces, so a grant on a deleted workspace cannot exist.
		//
		// This is the hottest authorization path in the product — nearly every
		// endpoint starts with "is the caller in this workspace" — and the probe
		// was a round-trip that could not change an answer.
		st.workspaceID = obj.ID
		st.path = "/" + pathSegment(obj) + "/"
		return st, nil

	case TypeChannel:
		err := c.pool.QueryRow(ctx,
			`SELECT workspace_id::text, type::text FROM channels WHERE id = $1`, obj.ID).
			Scan(&st.workspaceID, &st.channelType)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve channel object: %w", err)
		}
		st.path = "/" + pathSegment(WorkspaceObject(st.workspaceID)) + "/" + pathSegment(obj) + "/"
		return st, nil

	case TypeFile:
		// Both joins are constrained to the file's own workspace for the same
		// reason acl_object_expected constrains them: a cross-workspace
		// folder_id or message_id is corrupt data, and degrading to "uploader
		// only" is the fail-closed reading of it.
		//
		// The folder's path is read rather than derived. It is the folder's
		// CURRENT path — a folder that has been moved carries a different one —
		// and computing a second opinion here would make a moved subtree
		// authorize against where it used to be.
		var channelID, channelType, folderPath *string
		err := c.pool.QueryRow(ctx, `
			SELECT f.workspace_id::text, f.user_id::text, ch.id::text, ch.type::text, fo.path
			  FROM files f
			  LEFT JOIN acl_object fo ON fo.object_type  = 'folder'
			                         AND fo.object_id    = f.folder_id
			                         AND fo.workspace_id = f.workspace_id
			  LEFT JOIN messages m  ON m.id = f.message_id
			  LEFT JOIN channels ch ON ch.id = m.channel_id AND ch.workspace_id = f.workspace_id
			 WHERE f.id = $1`, obj.ID).
			Scan(&st.workspaceID, &st.ownerID, &channelID, &channelType, &folderPath)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve file object: %w", err)
		}
		if channelID != nil {
			st.parentChannel = *channelID
			if channelType != nil {
				st.channelType = *channelType
			}
		}
		// Path precedence matches acl_object_expected exactly: folder, then the
		// attached message's channel, then the workspace. It has to, because
		// grantedCapability walks these path segments looking for grants and a
		// path this function invented would look for them in the wrong place.
		wsSeg := "/" + pathSegment(WorkspaceObject(st.workspaceID)) + "/"
		switch {
		case folderPath != nil:
			st.path = *folderPath + pathSegment(obj) + "/"
		case channelID != nil:
			st.path = wsSeg + pathSegment(ChannelObject(*channelID)) + "/" + pathSegment(obj) + "/"
		default:
			st.path = wsSeg + pathSegment(obj) + "/"
		}
		return st, nil

	default:
		err := c.pool.QueryRow(ctx,
			`SELECT workspace_id::text, path FROM acl_object WHERE object_type = $1 AND object_id = $2`,
			obj.Type, obj.ID).Scan(&st.workspaceID, &st.path)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		if err != nil {
			return nil, fmt.Errorf("resolve object: %w", err)
		}
		return st, nil
	}
}

// ---------------------------------------------------------------------------
// The check
// ---------------------------------------------------------------------------

// Capability returns the effective capability a subject holds on an object:
//
//	max(capability implied by the legacy membership tables,
//	    explicit grants on the object,
//	    explicit grants on every ancestor)
//
// It returns a capability rather than a bool so a caller can distinguish "may
// read but not write" without a second query.
//
// A missing object, an unparseable id and a subject with no access are all
// (CapNone, nil) — never an error. err != nil means the database could not
// answer and is a 500; CapNone is a 403/404. Collapsing the two turns a
// database outage into an authorization bypass, or into a permanent lockout.
func (c *Checker) Capability(ctx context.Context, subject SubjectRef, obj ObjectRef) (Capability, error) {
	sub, subOK, err := subject.normalize()
	if err != nil {
		return CapNone, err
	}
	ref, objOK, err := obj.normalize()
	if err != nil {
		return CapNone, err
	}
	if !subOK || !objOK {
		return CapNone, nil
	}

	st, err := c.resolve(ctx, ref)
	if err != nil {
		return CapNone, err
	}
	if st == nil {
		return CapNone, nil
	}

	derived, err := c.derivedCapability(ctx, sub, st)
	if err != nil {
		return CapNone, err
	}
	if derived == CapAdmin {
		// Nothing can raise it further, and the grant query is the expensive
		// half of this function.
		return CapAdmin, nil
	}

	granted, err := c.grantedCapability(ctx, sub, st)
	if err != nil {
		return CapNone, err
	}
	if granted > derived {
		return granted, nil
	}
	return derived, nil
}

// Can is Capability plus a comparison. It exists so call sites read as the
// question they are asking.
//
// This is the method handlers call. err != nil is a 500 and !ok is a 403/404,
// never collapsed: a database fault that read as a denial would be an
// authorization decision made by an outage.
//
// Do not call it in a loop. Filtering a page of rows is KeysFor once, then
// FilterReadable — see the note at the top of this file.
func (c *Checker) Can(ctx context.Context, subject SubjectRef, obj ObjectRef, want Capability) (bool, error) {
	got, err := c.Capability(ctx, subject, obj)
	if err != nil {
		return false, err
	}
	return got.Implies(want), nil
}

// derivedCapability is the legacy layer: the capability the pre-ACL membership
// tables already imply. It is a transcription of the predicates authz.go
// enforces, and the dual-run comparison in compare.go is what proves the
// transcription is faithful.
//
// Only user subjects have one. A group holds exactly what it was granted.
func (c *Checker) derivedCapability(ctx context.Context, sub SubjectRef, st *objectState) (Capability, error) {
	if sub.Type != SubjectUser {
		return CapNone, nil
	}

	switch st.ref.Type {
	case TypeWorkspace:
		return c.workspaceCapability(ctx, st.workspaceID, sub.ID)

	case TypeChannel:
		return c.channelCapability(ctx, st.ref.ID, st.channelType, st.workspaceID, sub.ID)

	case TypeFile:
		// Exactly file.Handler.canRead: an attached file is readable by whoever
		// may read its channel; an unattached one (or one whose message was
		// hard-deleted) by its uploader alone. Workspace membership is
		// deliberately NOT enough — that gap is what let any member fetch a file
		// posted in a private channel given only its uuid.
		//
		// A Drive file falls through to CapNone here and gets its capability
		// from grantedCapability instead, via the workspace grant on the Drive
		// root. That is the whole reason Drive is expressed as a grant: this
		// function did not have to learn about folders, and the list path and
		// the decision path stayed one rule instead of two.
		if st.parentChannel != "" {
			return c.channelCapability(ctx, st.parentChannel, st.channelType, st.workspaceID, sub.ID)
		}
		if st.ownerID != "" && st.ownerID == sub.ID {
			return CapAdmin, nil
		}
		return CapNone, nil

	default:
		// An ACL-native type has no legacy layer: absence of a grant is denial,
		// which is fail-closed by construction.
		return CapNone, nil
	}
}

// workspaceCapability maps a workspace role onto the capability ladder.
//
// Note what it cannot express: owner and admin both map to CapAdmin, because
// the ladder is ordered and there is no rung between them. Workspace ownership
// stays a role check (IsWorkspaceOwner), and the dual-run comparison
// deliberately does not cover it.
func (c *Checker) workspaceCapability(ctx context.Context, workspaceID, userID string) (Capability, error) {
	role, err := c.WorkspaceRole(ctx, workspaceID, userID)
	if err != nil {
		return CapNone, err
	}
	switch role {
	case RoleOwner, RoleAdmin:
		return CapAdmin, nil
	case RoleMember:
		return CapWrite, nil
	case RoleGuest:
		return CapRead, nil
	default:
		return CapNone, nil
	}
}

// channelCapability maps channel membership onto the ladder, mirroring
// CanReadChannel: membership always suffices, and a public channel is readable
// by any member of its workspace — but not by an authenticated stranger, which
// is the gap that allowed cross-tenant browsing.
//
// channelType may be empty when the caller did not already have it, in which
// case it is looked up.
func (c *Checker) channelCapability(ctx context.Context, channelID, channelType, workspaceID, userID string) (Capability, error) {
	role, err := c.ChannelRole(ctx, channelID, userID)
	if err != nil {
		return CapNone, err
	}
	switch role {
	case ChannelRoleAdmin:
		return CapAdmin, nil
	case ChannelRoleMember:
		return CapWrite, nil
	}

	if channelType == "" {
		if err := c.pool.QueryRow(ctx, `SELECT type::text FROM channels WHERE id = $1`, channelID).
			Scan(&channelType); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return CapNone, nil
			}
			return CapNone, fmt.Errorf("get channel type: %w", err)
		}
	}
	if channelType != "public" {
		return CapNone, nil
	}
	member, err := c.isWorkspaceMember(ctx, workspaceID, userID)
	if err != nil || !member {
		return CapNone, err
	}
	return CapRead, nil
}

// grantedCapability is the ACL-native layer: the highest explicit grant this
// subject holds on the object or on any of its ancestors.
//
// Ancestors are matched by equality against the (at most MaxPathDepth) prefixes
// of the object's own path rather than by `a.path LIKE ... '%'`, so the lookup
// is an index probe per ancestor instead of a scan of acl_object.
func (c *Checker) grantedCapability(ctx context.Context, sub SubjectRef, st *objectState) (Capability, error) {
	paths := ancestorPaths(st.path)
	if len(paths) == 0 {
		return CapNone, nil
	}

	// A user holds their own grants AND every grant whose subject is a workspace
	// they belong to. The second half is what makes Drive shared: one
	// acl_grant(root folder, workspace, write) row, inherited down the path like
	// any other grant, instead of a special case in this function.
	//
	// The membership test is inside the query rather than a separate round trip,
	// so "the grants that apply to me" stays one statement — and so a user
	// removed from a workspace loses its grants in the same instant, with no
	// second read that could observe the old state.
	workspaceSubject := sub.Type == SubjectUser
	rows, err := c.pool.Query(ctx, `
		SELECT g.subject_type, g.capability
		  FROM acl_grant g
		  JOIN acl_object a ON a.object_type = g.object_type AND a.object_id = g.object_id
		 WHERE a.path = ANY($3)
		   AND (
		       (g.subject_type = $1 AND g.subject_id = $2)
		    OR ($4 AND g.subject_type = 'workspace'
		           AND g.subject_id = a.workspace_id
		           AND EXISTS (SELECT 1 FROM workspace_members wm
		                        WHERE wm.workspace_id = a.workspace_id
		                          AND wm.user_id = $2))
		   )`,
		sub.Type, sub.ID, paths, workspaceSubject)
	if err != nil {
		return CapNone, fmt.Errorf("list effective grants: %w", err)
	}
	defer rows.Close()

	best, workspaceBest := CapNone, CapNone
	for rows.Next() {
		var subjectType, raw string
		if err := rows.Scan(&subjectType, &raw); err != nil {
			return CapNone, fmt.Errorf("scan effective grant: %w", err)
		}
		// An unparseable capability is a row the CHECK constraint should have
		// refused. Ignoring it denies; treating it as admin would not.
		capability, ok := ParseCapability(raw)
		if !ok {
			continue
		}
		if subjectType == SubjectWorkspace {
			// Held back and capped below. A grant to "everyone in the workspace"
			// is addressed to a group whose members do NOT all deserve the same
			// thing: a guest is in the workspace. Applying it verbatim would let
			// a shared Drive hand write access to every guest account, which is
			// the opposite of what granting write to the team means.
			workspaceBest = max(workspaceBest, capability)
			continue
		}
		best = max(best, capability)
	}
	if err := rows.Err(); err != nil {
		return CapNone, fmt.Errorf("list effective grants: %w", err)
	}

	// A workspace grant is bounded by what the recipient's ROLE in that
	// workspace is worth: owner/admin admin, member write, guest read. So
	// granting write to the workspace gives members write and guests read,
	// and it can never promote anybody past their own role. A grant made to a
	// person directly is not capped — that is the point of granting to a person.
	if workspaceBest != CapNone && st.workspaceID != "" {
		role, err := c.workspaceCapability(ctx, st.workspaceID, sub.ID)
		if err != nil {
			return CapNone, err
		}
		best = max(best, min(workspaceBest, role))
	}
	return best, nil
}

// ancestorPaths returns every prefix of a path, self included, shortest first.
// It returns nil past MaxPathDepth rather than truncating: a truncated ancestor
// list silently drops the grants nearest the root.
func ancestorPaths(path string) []string {
	segs := pathSegments(path)
	if len(segs) == 0 || len(segs) > MaxPathDepth {
		return nil
	}
	out := make([]string, 0, len(segs))
	prefix := "/"
	for _, seg := range segs {
		prefix += seg + "/"
		out = append(out, prefix)
	}
	return out
}

// ---------------------------------------------------------------------------
// The list path
// ---------------------------------------------------------------------------

// KeysFor resolves the set of access keys a subject holds in a workspace. This
// is the query side of the access-key model, and it is what makes list and
// search endpoints viable: resolve once per request, then filter.
//
// The keys are exactly the strings internal/search's validKey accepts, because
// the Meilisearch filter is built by string concatenation and a key that fails
// validation is dropped — and a dropped narrowing term widens the query, which
// for a tenancy filter is a cross-tenant leak.
//
//	w-<workspace>  the subject is a member of the workspace
//	u-<subject>    objects shared with, or owned by, this subject
//	c-<channel>    one per channel the subject may read
//	f-<folder>     one per folder the subject may read (nothing emits these yet)
//
// A non-member gets an empty slice, which every caller must render as "no
// results" rather than as "no filter".
//
// The channel arm is answered from channel_members/channels — the tables that
// are authoritative for channels — and NOT from acl_key. That survived step 4
// on purpose, and the reasoning is worth stating because it is the one place
// the cutover deliberately did not go all the way.
//
// A key set is a NARROWING term. Every consumer builds a filter from it, so a
// key that should be there and is not costs somebody a search result, while a
// key that is there and should not be is a cross-tenant read. The two failures
// are not symmetric, and staleness produces the second one: a caller removed
// from a private channel keeps its c- key until the materialization catches up.
//
// acl_key's derived half (workspaces, channels, files) is maintained by the
// worker's hourly reconcile, not by the write that changed it — no handler
// writes an acl_object row when a channel is created or a member joins, because
// those rows are computed from acl_object_expected and a hand-placed one would
// be reverted by the next reconcile. An hour of staleness on a narrowing filter
// is not a trade worth making to save one indexed query.
//
// Flipping this arm needs the derived materialization to be transactional with
// the thing it is derived from — a trigger on channels/channel_members/files,
// or a hook on every one of the eight write paths that touch them — which is a
// change with its own failure mode (a missed hook is a channel that is
// invisible or unguarded depending on which arm reads it) and belongs in its
// own commit with its own migration. Until then the acl_key arm stays
// restricted to container types the ACL layer owns outright, where the
// materialization is rewritten in the same transaction as the grant that
// changed it.
//
// Nothing else in the cutover depends on that decision: no authorization
// decision about a workspace, channel or file reads acl_object or acl_key at
// all. Capability resolves those three types from the legacy tables directly
// (see resolve and derivedCapability), so an object created one second ago is
// authorized correctly with no row of its own — which is exactly why a missing
// row cannot widen anything.
func (c *Checker) KeysFor(ctx context.Context, subject SubjectRef, workspaceID string) ([]string, error) {
	sub, ok, err := subject.normalize()
	if err != nil {
		return nil, err
	}
	if !ok {
		return []string{}, nil
	}
	if sub.Type != SubjectUser {
		// Group and link subjects have no workspace membership to resolve, and
		// guessing one would be a tenancy decision made by accident.
		return nil, fmt.Errorf("KeysFor: unsupported subject type %q", sub.Type)
	}
	wsID, ok := canonicalUUID(workspaceID)
	if !ok {
		return []string{}, nil
	}

	member, err := c.isWorkspaceMember(ctx, wsID, sub.ID)
	if err != nil {
		return nil, err
	}
	if !member {
		return []string{}, nil
	}

	keys := []string{WorkspaceKey(wsID), sub.Key()}

	channels, err := c.readableChannelIDs(ctx, wsID, sub.ID)
	if err != nil {
		return nil, err
	}
	for _, id := range channels {
		if k := ContainerKey(ChannelObject(id)); k != "" {
			keys = append(keys, k)
		}
	}

	native, err := c.nativeContainerKeys(ctx, wsID, keys)
	if err != nil {
		return nil, err
	}
	keys = append(keys, native...)

	return dedupeSorted(keys), nil
}

// aclNativeContainerTypes are the container types whose readability is decided
// by acl_key alone. TypeChannel is absent on purpose — see KeysFor.
var aclNativeContainerTypes = []string{TypeFolder}

// nativeContainerKeys expands the identity keys a subject holds into the
// container keys of every ACL-native container those identity keys can read.
//
// One indexed query against idx_acl_key_lookup, not one per container. Nesting
// is already handled: acl_key materializes inherited grants, so a folder three
// levels below a shared one carries the same identity key its ancestor was
// granted to.
func (c *Checker) nativeContainerKeys(ctx context.Context, workspaceID string, identityKeys []string) ([]string, error) {
	rows, err := c.pool.Query(ctx, `
		SELECT DISTINCT o.object_type, o.object_id::text
		  FROM acl_key k
		  JOIN acl_object o ON o.object_type = k.object_type AND o.object_id = k.object_id
		 WHERE k.key = ANY($1) AND o.workspace_id = $2 AND o.object_type = ANY($3)`,
		identityKeys, workspaceID, aclNativeContainerTypes)
	if err != nil {
		return nil, fmt.Errorf("list readable containers: %w", err)
	}
	defer rows.Close()

	out := []string{}
	for rows.Next() {
		var objType, objID string
		if err := rows.Scan(&objType, &objID); err != nil {
			return nil, fmt.Errorf("scan readable container: %w", err)
		}
		if k := ContainerKey(ObjectRef{Type: objType, ID: objID}); k != "" {
			out = append(out, k)
		}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list readable containers: %w", err)
	}
	return out, nil
}

// FilterReadable keeps the ids a key set may read, preserving input order.
//
// This is the list path, and it is one indexed query for the whole page. The
// alternative — Can per row — is the N+1 this whole design exists to prevent,
// and it reads as correct, which is why the batch call is the one with the
// convenient signature.
//
// It answers from acl_key, so it is only meaningful for the object types the
// ACL layer owns outright — the ones whose keys are rewritten in the same
// transaction as the grant that changed them. For a channel or a file, whose
// materialization is reconciled hourly rather than transactionally, ask Can (one
// object) or KeysFor (a whole workspace's worth); see the note on KeysFor for
// why a stale key here would widen rather than narrow.
func (c *Checker) FilterReadable(ctx context.Context, keys []string, objectType string, ids []string) ([]string, error) {
	if err := validObjectType(objectType); err != nil {
		return nil, err
	}
	if len(keys) == 0 || len(ids) == 0 {
		return []string{}, nil
	}

	canonical := make([]string, 0, len(ids))
	for _, id := range ids {
		if c, ok := canonicalUUID(id); ok {
			canonical = append(canonical, c)
		}
	}
	if len(canonical) == 0 {
		return []string{}, nil
	}

	rows, err := c.pool.Query(ctx, `
		SELECT DISTINCT object_id::text
		  FROM acl_key
		 WHERE object_type = $1 AND object_id = ANY($2::uuid[]) AND key = ANY($3)`,
		objectType, canonical, keys)
	if err != nil {
		return nil, fmt.Errorf("filter readable objects: %w", err)
	}
	defer rows.Close()

	allowed := map[string]bool{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("scan readable object: %w", err)
		}
		allowed[id] = true
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("filter readable objects: %w", err)
	}

	out := make([]string, 0, len(ids))
	for _, id := range ids {
		if canon, ok := canonicalUUID(id); ok && allowed[canon] {
			out = append(out, id)
		}
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// The audit path
// ---------------------------------------------------------------------------

// GrantsFor lists every explicit grant a subject holds — "this person left the
// company, what did they have access to". It reports the grants themselves, not
// their subtrees: a grant on a folder is one row, and the objects it reaches
// are the subtree of that folder.
//
// It does not include capability implied by workspace or channel membership;
// that is what WorkspaceRole and ChannelRole already answer.
func (c *Checker) GrantsFor(ctx context.Context, subject SubjectRef) ([]Grant, error) {
	sub, ok, err := subject.normalize()
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Grant{}, nil
	}

	rows, err := c.pool.Query(ctx, `
		SELECT g.object_type, g.object_id::text, g.capability,
		       o.workspace_id::text, COALESCE(g.granted_by::text, ''),
		       EXTRACT(EPOCH FROM g.created_at)::bigint
		  FROM acl_grant g
		  JOIN acl_object o ON o.object_type = g.object_type AND o.object_id = g.object_id
		 WHERE g.subject_type = $1 AND g.subject_id = $2
		 ORDER BY o.workspace_id, g.object_type, g.object_id`,
		sub.Type, sub.ID)
	if err != nil {
		return nil, fmt.Errorf("list grants for subject: %w", err)
	}
	defer rows.Close()
	return scanGrants(rows, func(g *Grant) { g.Subject = sub })
}

// SubjectsOf lists everyone who holds an explicit grant that reaches an object
// — its own grants and every ancestor's, because a grant on a parent folder is
// just as much an answer to "who can see this" as a grant on the file.
//
// Grant.Object names where each grant lives, so a caller can tell an inherited
// share from a direct one.
func (c *Checker) SubjectsOf(ctx context.Context, obj ObjectRef) ([]Grant, error) {
	ref, ok, err := obj.normalize()
	if err != nil {
		return nil, err
	}
	if !ok {
		return []Grant{}, nil
	}
	st, err := c.resolve(ctx, ref)
	if err != nil {
		return nil, err
	}
	if st == nil {
		return []Grant{}, nil
	}
	paths := ancestorPaths(st.path)
	if len(paths) == 0 {
		return []Grant{}, nil
	}

	rows, err := c.pool.Query(ctx, `
		SELECT g.object_type, g.object_id::text, g.capability,
		       o.workspace_id::text, COALESCE(g.granted_by::text, ''),
		       EXTRACT(EPOCH FROM g.created_at)::bigint,
		       g.subject_type, g.subject_id::text
		  FROM acl_grant g
		  JOIN acl_object o ON o.object_type = g.object_type AND o.object_id = g.object_id
		 WHERE o.path = ANY($1)
		 ORDER BY length(o.path), g.subject_type, g.subject_id`, paths)
	if err != nil {
		return nil, fmt.Errorf("list subjects of object: %w", err)
	}
	defer rows.Close()

	out := []Grant{}
	for rows.Next() {
		var g Grant
		var capability, subjectType, subjectID string
		if err := rows.Scan(&g.Object.Type, &g.Object.ID, &capability,
			&g.WorkspaceID, &g.GrantedBy, &g.CreatedAt, &subjectType, &subjectID); err != nil {
			return nil, fmt.Errorf("scan object subject: %w", err)
		}
		g.Capability, _ = ParseCapability(capability)
		g.Subject = SubjectRef{Type: subjectType, ID: subjectID}
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list subjects of object: %w", err)
	}
	return out, nil
}

func scanGrants(rows pgx.Rows, fill func(*Grant)) ([]Grant, error) {
	out := []Grant{}
	for rows.Next() {
		var g Grant
		var capability string
		if err := rows.Scan(&g.Object.Type, &g.Object.ID, &capability,
			&g.WorkspaceID, &g.GrantedBy, &g.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan grant: %w", err)
		}
		g.Capability, _ = ParseCapability(capability)
		fill(&g)
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list grants: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Revocation latency
// ---------------------------------------------------------------------------

// Revoker is the live half of a revocation.
//
// Deleting an acl_grant row and rewriting acl_key settles every FUTURE request.
// It settles nothing about a WebSocket subscription or an editing session that
// was authorized before the revocation and whose delivery is gated on an
// in-memory map written at subscribe time — plan 00's "revocation latency"
// risk. Until the cutover that gap was theoretical for grants, because no
// handler consulted a grant; now that Can is what handlers enforce, a grant is
// access and withdrawing one has to reach the sockets.
//
// It is deliberately expressed in (subject, object) terms rather than in
// channel ids: internal/authz must not know that a channel is a hub
// subscription and a document is a room. The composition root maps object types
// onto ws.Hub.RevokeChannelSubscription / collab.Service.RevokeAccess.
//
// Both methods are best effort and are called AFTER the transaction commits.
// The rows are already gone by then, so a hub that cannot be reached must never
// turn a successful revocation into a failed request — and calling before the
// commit would revoke access that a rollback then restores.
type Revoker interface {
	// RevokeObjectForSubject cuts one subject out of one object.
	RevokeObjectForSubject(subject SubjectRef, obj ObjectRef)
	// RevokeObjectForAll cuts everyone out, for a change that alters what the
	// whole object inherits.
	RevokeObjectForAll(obj ObjectRef)
}

// maxLiveRevocations bounds how many objects one mutation will walk. A grant on
// a workspace-level container can reach every channel in the tenant, and a
// revocation must not turn into an unbounded fan-out inside a request. Past the
// cap the socket's own periodic membership recheck is what closes the gap, so
// the failure mode is latency rather than leaked access — but it is logged,
// because minutes of retained access is worth knowing about.
const maxLiveRevocations = 512

// liveDescendants lists the objects in a subtree that a subject can hold an
// open connection to. It runs inside the mutation's transaction so it sees the
// paths the mutation just wrote.
func liveDescendants(ctx context.Context, tx pgx.Tx, prefix string) ([]ObjectRef, bool, error) {
	rows, err := tx.Query(ctx, `
		SELECT object_type, object_id::text
		  FROM acl_object
		 WHERE path LIKE $1 || '%' AND object_type = ANY($2)
		 ORDER BY path
		 LIMIT $3`, prefix, liveTypes, maxLiveRevocations+1)
	if err != nil {
		return nil, false, fmt.Errorf("list live descendants: %w", err)
	}
	defer rows.Close()

	out := []ObjectRef{}
	for rows.Next() {
		var ref ObjectRef
		if err := rows.Scan(&ref.Type, &ref.ID); err != nil {
			return nil, false, fmt.Errorf("scan live descendant: %w", err)
		}
		out = append(out, ref)
	}
	if err := rows.Err(); err != nil {
		return nil, false, fmt.Errorf("list live descendants: %w", err)
	}
	if len(out) > maxLiveRevocations {
		return out[:maxLiveRevocations], true, nil
	}
	return out, false, nil
}

// revokeLive drives the Revoker over a set of objects. subject is nil for a
// revocation that affects everybody.
func (c *Checker) revokeLive(subject *SubjectRef, refs []ObjectRef, truncated bool, cause string) {
	if c.revoker == nil || len(refs) == 0 {
		return
	}
	if truncated {
		slog.Default().Warn("authz: live revocation fan-out truncated; "+
			"connections beyond the cap keep access until their periodic recheck",
			"cause", cause, "limit", maxLiveRevocations)
	}
	for _, ref := range refs {
		if subject == nil {
			c.revoker.RevokeObjectForAll(ref)
			continue
		}
		c.revoker.RevokeObjectForSubject(*subject, ref)
	}
}

// ---------------------------------------------------------------------------
// Mutation — every one of these rewrites acl_key in the same transaction
// ---------------------------------------------------------------------------
//
// acl_key is denormalized AUTHORIZATION state. Drift in it is not a stale
// cache: it is someone seeing something they should not, or not seeing
// something they should. So the rewrite is never deferred to a job, never
// best-effort, and never outside the transaction that changed the thing it is
// derived from.

// Register creates or repositions the acl_object row for an ACL-native object
// (a folder, a doc, a sheet — anything Drive and the editors introduce). The
// parent must already be registered and decides the workspace: an object cannot
// be created into a tenancy of its own choosing.
//
// Derived types are refused. workspaces, channels and files get their rows from
// acl_object_expected via Rebuild, and a hand-placed row for one of them would
// be silently reverted by the next reconcile.
func (c *Checker) Register(ctx context.Context, obj, parent ObjectRef) error {
	ref, ok, err := obj.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("register %s: invalid object id", obj)
	}
	if derivedTypes[ref.Type] {
		return fmt.Errorf("register %s: %w", ref, ErrDerivedType)
	}
	parentRef, ok, err := parent.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("register %s: invalid parent id", obj)
	}
	if parentRef == ref {
		return fmt.Errorf("register %s: an object cannot be its own parent", ref)
	}

	return database.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		return registerIn(ctx, tx, ref, parentRef)
	})
}

// RegisterTx is Register inside the CALLER's transaction.
//
// Drive needs it: creating a folder writes the drive_folders row and this in one
// commit. An acl_object row for a folder that was rolled back would be an
// authorization decision about something that does not exist, and a folder with
// no acl_object row is readable by nobody — both are worse than the write
// failing.
func (c *Checker) RegisterTx(ctx context.Context, tx pgx.Tx, obj, parent ObjectRef) error {
	ref, parentRef, err := normalizeRegister(obj, parent)
	if err != nil {
		return err
	}
	return registerIn(ctx, tx, ref, parentRef)
}

// normalizeRegister is the validation both entry points share. Duplicating it
// would mean one of them eventually accepting what the other refuses.
func normalizeRegister(obj, parent ObjectRef) (ref, parentRef ObjectRef, err error) {
	ref, ok, err := obj.normalize()
	if err != nil {
		return ref, parentRef, err
	}
	if !ok {
		return ref, parentRef, fmt.Errorf("register %s: invalid object id", obj)
	}
	if derivedTypes[ref.Type] {
		return ref, parentRef, fmt.Errorf("register %s: %w", ref, ErrDerivedType)
	}
	parentRef, ok, err = parent.normalize()
	if err != nil {
		return ref, parentRef, err
	}
	if !ok {
		return ref, parentRef, fmt.Errorf("register %s: invalid parent id", obj)
	}
	if parentRef == ref {
		return ref, parentRef, fmt.Errorf("register %s: an object cannot be its own parent", ref)
	}
	return ref, parentRef, nil
}

func registerIn(ctx context.Context, tx pgx.Tx, ref, parentRef ObjectRef) error {
	{
		parentState, err := lockObject(ctx, tx, parentRef)
		if err != nil {
			return err
		}
		if parentState == nil && derivedTypes[parentRef.Type] {
			// The parent is a workspace, channel or file whose acl_object row
			// has not been materialized yet.
			//
			// That is a NORMAL state, not corruption: derived rows come from
			// acl_object_expected via Rebuild, which runs hourly, so a workspace
			// created five minutes ago has no row. Failing here meant the first
			// folder anybody created in a new workspace failed — and it would
			// have started working on its own an hour later, which is the worst
			// possible shape for a bug report.
			//
			// Materializing it from the view is not a second opinion: the view
			// IS the definition, and Rebuild would write exactly this row.
			if err := materializeDerived(ctx, tx, parentRef); err != nil {
				return err
			}
			parentState, err = lockObject(ctx, tx, parentRef)
			if err != nil {
				return err
			}
		}
		if parentState == nil {
			return fmt.Errorf("register %s under %s: %w", ref, parentRef, ErrNotRegistered)
		}

		path := buildPath(parentState.path, ref)
		if pathDepth(path) > MaxPathDepth {
			return fmt.Errorf("register %s: path would be %d segments deep, the limit is %d",
				ref, pathDepth(path), MaxPathDepth)
		}

		if _, err := tx.Exec(ctx, `
			INSERT INTO acl_object (object_type, object_id, workspace_id, path)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (object_type, object_id) DO UPDATE
			   SET workspace_id = EXCLUDED.workspace_id, path = EXCLUDED.path, updated_at = NOW()`,
			ref.Type, ref.ID, parentState.workspaceID, path); err != nil {
			return fmt.Errorf("register object: %w", err)
		}
		return rewriteSubtreeKeys(ctx, tx, path)
	}
}

// Grant records an explicit (subject, object, capability). One row per
// (object, subject): granting again replaces the capability rather than adding
// a second opinion about which one is current.
func (c *Checker) Grant(ctx context.Context, actor, subject SubjectRef, obj ObjectRef, capability Capability) error {
	sub, ref, stored, grantedBy, err := normalizeGrant(actor, subject, obj, capability)
	if err != nil {
		return err
	}

	if err := database.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		return grantIn(ctx, tx, ref, sub, capability, stored, grantedBy)
	}); err != nil {
		return err
	}

	c.audit(ctx, actor, "acl.granted", ref, &sub,
		map[string]interface{}{"capability": string(capability)})
	return nil
}

// GrantTx is Grant inside the CALLER's transaction.
//
// It exists for the one grant Drive writes as part of creating something: the
// workspace grant on a new Drive root. That grant is what makes Drive visible
// at all, so a root committed without it would be a Drive nobody could open —
// and the repair is a manual SQL statement, because nothing would report it.
//
// It does NOT write the audit entry. The audit sink is asynchronous and its
// entry would describe a grant a rollback might discard; the caller records the
// action it is performing, which is "created a Drive", not "granted".
func (c *Checker) GrantTx(ctx context.Context, tx pgx.Tx, actor, subject SubjectRef, obj ObjectRef, capability Capability) error {
	sub, ref, stored, grantedBy, err := normalizeGrant(actor, subject, obj, capability)
	if err != nil {
		return err
	}
	return grantIn(ctx, tx, ref, sub, capability, stored, grantedBy)
}

// ErrNativeType is returned by MaterializeTx for an ACL-native object. Those
// have no expected state to compute — Register wrote their row — so asking to
// materialize one is a caller confusing the two halves of the model.
var ErrNativeType = errors.New("object type is ACL-native; its row is written by Register, not computed")

// MaterializeTx brings ONE derived object's acl_object and acl_key rows up to
// date, inside the caller's transaction.
//
// It exists because "derived" describes where the rows come from, not when they
// arrive. They come from acl_object_expected via Rebuild, which runs hourly — so
// a file created a second ago has no acl_object row and no acl_key rows, and
// every LIST path filters on acl_key. The object is openable (Capability
// resolves a file from files.folder_id directly) and invisible in the listing it
// belongs to, and then it appears on its own up to an hour later.
//
// That is the listing-versus-opening split, and it is the exact failure
// docs/plans/README.md ruling 5 names. Any handler that creates or relocates a
// derived object calls this in the same transaction.
//
// It is a FILTER over the same views Rebuild uses, never a second computation of
// the path: a hand-written path here would disagree with the view and the drift
// verifier would report its own disagreement as production drift.
func MaterializeTx(ctx context.Context, tx pgx.Tx, obj ObjectRef) error {
	ref, ok, err := obj.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("materialize %s: invalid object id", obj)
	}
	if !derivedTypes[ref.Type] {
		return fmt.Errorf("materialize %s: %w", ref, ErrNativeType)
	}
	return materializeDerived(ctx, tx, ref)
}

// materializeDerived inserts one derived object's acl_object row from
// acl_object_expected, and its keys from acl_key_expected.
//
// It is the single-object equivalent of Rebuild, written as a filter over the
// same views so the two cannot disagree. Nothing here computes a path.
func materializeDerived(ctx context.Context, tx pgx.Tx, ref ObjectRef) error {
	if _, err := tx.Exec(ctx, `
		INSERT INTO acl_object (object_type, object_id, workspace_id, path)
		SELECT object_type, object_id, workspace_id, path
		  FROM acl_object_expected
		 WHERE object_type = $1 AND object_id = $2
		    ON CONFLICT (object_type, object_id) DO UPDATE
		   SET workspace_id = EXCLUDED.workspace_id,
		       path         = EXCLUDED.path,
		       updated_at   = NOW()
		 WHERE acl_object.workspace_id IS DISTINCT FROM EXCLUDED.workspace_id
		    OR acl_object.path         IS DISTINCT FROM EXCLUDED.path`,
		ref.Type, ref.ID); err != nil {
		return fmt.Errorf("materialize %s: %w", ref, err)
	}
	// DO UPDATE, not DO NOTHING: this is also the MOVE path. A file moved to
	// another folder keeps its row and changes its path, and an insert that
	// skipped the conflict would leave it authorized against the folder it
	// used to be in — visible in a listing it has left.

	// Keys are reconciled, not merely added. A move makes the OLD container key
	// wrong, and a key that is only ever inserted is a key that never goes away:
	// the file would keep appearing in its previous folder's listing forever.
	// Delete-then-insert rather than truncate, so a concurrent reader never sees
	// an object with no keys and concludes it is unreadable.
	if _, err := tx.Exec(ctx, `
		DELETE FROM acl_key k
		 WHERE k.object_type = $1 AND k.object_id = $2
		   AND NOT EXISTS (SELECT 1 FROM acl_key_expected e
		                    WHERE e.object_type = k.object_type
		                      AND e.object_id   = k.object_id
		                      AND e.key         = k.key)`,
		ref.Type, ref.ID); err != nil {
		return fmt.Errorf("prune stale keys for %s: %w", ref, err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO acl_key (object_type, object_id, key)
		SELECT object_type, object_id, key
		  FROM acl_key_expected
		 WHERE object_type = $1 AND object_id = $2
		    ON CONFLICT DO NOTHING`,
		ref.Type, ref.ID); err != nil {
		return fmt.Errorf("materialize keys for %s: %w", ref, err)
	}
	return nil
}

// normalizeGrant is the validation both entry points share. Duplicating it
// would mean one of them eventually accepting what the other refuses.
func normalizeGrant(actor, subject SubjectRef, obj ObjectRef, capability Capability) (
	sub SubjectRef, ref ObjectRef, stored string, grantedBy *string, err error) {
	sub, ok, err := subject.normalize()
	if err != nil {
		return sub, ref, "", nil, err
	}
	if !ok {
		return sub, ref, "", nil, fmt.Errorf("grant on %s: invalid subject id", obj)
	}
	ref, ok, err = obj.normalize()
	if err != nil {
		return sub, ref, "", nil, err
	}
	if !ok {
		return sub, ref, "", nil, fmt.Errorf("grant to %s: invalid object id", sub)
	}
	stored, err = capability.storable()
	if err != nil {
		return sub, ref, "", nil, fmt.Errorf("grant %s on %s: %w", sub, ref, err)
	}

	// granted_by is audit, not authorization: an unparseable or absent actor
	// records NULL rather than refusing, because losing the audit trail is
	// worse than losing the attribution.
	if actor.Type == SubjectUser {
		if id, ok := canonicalUUID(actor.ID); ok {
			grantedBy = &id
		}
	}
	return sub, ref, stored, grantedBy, nil
}

func grantIn(ctx context.Context, tx pgx.Tx, ref ObjectRef, sub SubjectRef,
	capability Capability, stored string, grantedBy *string) error {
	st, err := lockObject(ctx, tx, ref)
	if err != nil {
		return err
	}
	if st == nil {
		return fmt.Errorf("grant %s on %s: %w", capability, ref, ErrNotRegistered)
	}

	if _, err := tx.Exec(ctx, `
		INSERT INTO acl_grant (object_type, object_id, subject_type, subject_id, capability, granted_by)
		VALUES ($1, $2, $3, $4, $5, $6)
		ON CONFLICT (object_type, object_id, subject_type, subject_id) DO UPDATE
		   SET capability = EXCLUDED.capability,
		       granted_by = EXCLUDED.granted_by,
		       updated_at = NOW()`,
		ref.Type, ref.ID, sub.Type, sub.ID, stored, grantedBy); err != nil {
		return fmt.Errorf("write grant: %w", err)
	}
	return rewriteSubtreeKeys(ctx, tx, st.path)
}

// Revoke removes an explicit grant. Revoking one that does not exist is not an
// error: the caller asked for a state, and the state already holds.
//
// It removes the grant only. Capability implied by workspace or channel
// membership is not a grant and is not revocable here — removing someone from a
// channel is still channel.Repository's job.
//
// After the transaction commits it drives the Revoker over every live object in
// the subtree, so a subscription authorized before the revocation stops
// delivering rather than surviving until its periodic recheck.
func (c *Checker) Revoke(ctx context.Context, actor, subject SubjectRef, obj ObjectRef) error {
	sub, ok, err := subject.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("revoke on %s: invalid subject id", obj)
	}
	ref, ok, err := obj.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("revoke from %s: invalid object id", sub)
	}

	var live []ObjectRef
	var truncated bool
	if err := database.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		st, err := lockObject(ctx, tx, ref)
		if err != nil {
			return err
		}
		if st == nil {
			// No object means no grant to remove and no keys to rewrite.
			return nil
		}
		if _, err := tx.Exec(ctx, `
			DELETE FROM acl_grant
			 WHERE object_type = $1 AND object_id = $2 AND subject_type = $3 AND subject_id = $4`,
			ref.Type, ref.ID, sub.Type, sub.ID); err != nil {
			return fmt.Errorf("delete grant: %w", err)
		}
		if err := rewriteSubtreeKeys(ctx, tx, st.path); err != nil {
			return err
		}
		live, truncated, err = liveDescendants(ctx, tx, st.path)
		return err
	}); err != nil {
		return err
	}

	c.revokeLive(&sub, live, truncated, "revoke "+sub.String()+" on "+ref.String())
	c.audit(ctx, actor, "acl.revoked", ref, &sub, nil)
	return nil
}

// Move reparents an object and its whole subtree. The path rewrite is a single
// prefix UPDATE — the reason the hierarchy is a materialized path and not a
// closure table.
//
// Four things it refuses, all of them tenancy or integrity rather than policy:
// a derived type (its path is computed, not stored), a cross-workspace move
// (acl_object.workspace_id is authoritative and a move must never widen
// tenancy), a move into the object's own subtree (a cycle), and a move that
// would push any descendant past MaxPathDepth.
//
// A move changes what the whole subtree inherits, in both directions and for
// every subject at once — there is no "who lost access" to compute, only "what
// everybody inherits is now different". So it drives the Revoker for ALL rather
// than for a subject: every live session in the moved subtree is dropped and
// re-authorizes against the new position. That is the plan's revocation-latency
// risk, closed.
func (c *Checker) Move(ctx context.Context, actor SubjectRef, obj, newParent ObjectRef) error {
	ref, ok, err := obj.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("move: invalid object id")
	}
	parentRef, ok, err := newParent.normalize()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("move %s: invalid parent id", ref)
	}
	if derivedTypes[ref.Type] {
		return fmt.Errorf("move %s: %w", ref, ErrDerivedType)
	}
	if ref == parentRef {
		return fmt.Errorf("move %s: %w", ref, ErrMoveCycle)
	}

	var live []ObjectRef
	var truncated bool
	if err := database.WithTx(ctx, c.pool, func(tx pgx.Tx) error {
		// Lock in a stable order so two concurrent moves cannot deadlock on
		// each other's rows.
		first, second := ref, parentRef
		if first.String() > second.String() {
			first, second = second, first
		}
		states := map[ObjectRef]*objectState{}
		for _, r := range []ObjectRef{first, second} {
			st, err := lockObject(ctx, tx, r)
			if err != nil {
				return err
			}
			if st == nil {
				return fmt.Errorf("move %s under %s: %s: %w", ref, parentRef, r, ErrNotRegistered)
			}
			states[r] = st
		}
		src, dst := states[ref], states[parentRef]

		if src.workspaceID != dst.workspaceID {
			return fmt.Errorf("move %s under %s: %w: %s and %s",
				ref, parentRef, ErrCrossWorkspaceMove, src.workspaceID, dst.workspaceID)
		}
		if hasPrefixPath(dst.path, src.path) {
			return fmt.Errorf("move %s under %s: %w", ref, parentRef, ErrMoveCycle)
		}

		newPath := buildPath(dst.path, ref)

		// The deepest descendant decides whether the move fits. Checking only
		// the object itself would let a move push its children past the cap and
		// leave the UPDATE to fail on a CHECK constraint with no explanation.
		var deepest int
		if err := tx.QueryRow(ctx, `
			SELECT COALESCE(MAX(length(path) - length(replace(path, '/', ''))), 0) - 1
			  FROM acl_object WHERE path LIKE $1 || '%'`, src.path).Scan(&deepest); err != nil {
			return fmt.Errorf("measure subtree depth: %w", err)
		}
		if grown := deepest - pathDepth(src.path) + pathDepth(newPath); grown > MaxPathDepth {
			return fmt.Errorf("move %s under %s: %w: %d segments, the limit is %d",
				ref, parentRef, ErrMoveTooDeep, grown, MaxPathDepth)
		}

		if _, err := tx.Exec(ctx, `
			UPDATE acl_object
			   SET path = $2 || substring(path FROM $3::int), updated_at = NOW()
			 WHERE path LIKE $1 || '%'`,
			src.path, newPath, len(src.path)+1); err != nil {
			return fmt.Errorf("move subtree: %w", err)
		}
		if err := rewriteSubtreeKeys(ctx, tx, newPath); err != nil {
			return err
		}
		live, truncated, err = liveDescendants(ctx, tx, newPath)
		return err
	}); err != nil {
		return err
	}

	c.revokeLive(nil, live, truncated, "move "+ref.String()+" under "+parentRef.String())
	c.audit(ctx, actor, "acl.moved", ref, nil,
		map[string]interface{}{"new_parent_type": string(parentRef.Type), "new_parent_id": parentRef.ID})
	return nil
}

// lockObject reads an acl_object row FOR UPDATE, returning (nil, nil) when it
// does not exist. The lock is what serialises a Move against a concurrent Grant
// on the same subtree — without it the grant's key rewrite could be computed
// against the pre-move paths and committed after the move.
func lockObject(ctx context.Context, tx pgx.Tx, ref ObjectRef) (*objectState, error) {
	st := &objectState{ref: ref}
	err := tx.QueryRow(ctx, `
		SELECT workspace_id::text, path FROM acl_object
		 WHERE object_type = $1 AND object_id = $2 FOR UPDATE`,
		ref.Type, ref.ID).Scan(&st.workspaceID, &st.path)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("lock object %s: %w", ref, err)
	}
	return st, nil
}

// rewriteSubtreeKeys recomputes acl_key for every object at or under prefix,
// from acl_key_expected — the single definition the migration's backfill and
// the drift verifier also use. A second, hand-written copy of this logic in Go
// would be a verifier that reports its own disagreement as production drift.
//
// Delete-then-insert rather than truncate-and-rebuild: the transaction holds,
// so no reader ever observes an object with no keys and concludes it is
// unreadable.
//
// prefix is a stored path, and acl_object's CHECK constraints guarantee it
// contains no LIKE metacharacter — that is why this concatenates rather than
// escapes.
func rewriteSubtreeKeys(ctx context.Context, tx pgx.Tx, prefix string) error {
	if _, err := tx.Exec(ctx, `
		DELETE FROM acl_key k
		 USING acl_object o
		 WHERE o.object_type = k.object_type AND o.object_id = k.object_id
		   AND o.path LIKE $1 || '%'`, prefix); err != nil {
		return fmt.Errorf("clear subtree keys: %w", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO acl_key (object_type, object_id, key)
		SELECT e.object_type, e.object_id, e.key
		  FROM acl_key_expected e
		  JOIN acl_object o ON o.object_type = e.object_type AND o.object_id = e.object_id
		 WHERE o.path LIKE $1 || '%'
		    ON CONFLICT DO NOTHING`, prefix); err != nil {
		return fmt.Errorf("materialize subtree keys: %w", err)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func dedupeSorted(in []string) []string {
	seen := make(map[string]bool, len(in))
	out := make([]string, 0, len(in))
	for _, s := range in {
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	sort.Strings(out)
	return out
}

// KeysForObject returns the access keys materialized for one object.
//
// It is the read side of acl_key, and it exists for the search indexer: a
// document's ACL has to be the keys the database actually holds, not a second
// derivation of them. A derivation that disagrees with the table is either a
// leak (wider) or a blank page (narrower), and both look like the search being
// broken rather than like an authorization bug.
//
// An object with no rows returns an empty slice, not an error. That is a real
// state — an object whose materialization has not run — and the caller's
// fail-closed answer is to index nothing rather than to index everything.
func (c *Checker) KeysForObject(ctx context.Context, objectType, objectID string) ([]string, error) {
	if err := validObjectType(objectType); err != nil {
		return nil, err
	}
	id, ok := canonicalUUID(objectID)
	if !ok {
		return []string{}, nil
	}
	rows, err := c.pool.Query(ctx,
		`SELECT key FROM acl_key WHERE object_type = $1 AND object_id = $2 ORDER BY key`,
		objectType, id)
	if err != nil {
		return nil, fmt.Errorf("read access keys for %s:%s: %w", objectType, id, err)
	}
	defer rows.Close()

	keys := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			return nil, fmt.Errorf("scan access key: %w", err)
		}
		keys = append(keys, k)
	}
	return keys, rows.Err()
}
