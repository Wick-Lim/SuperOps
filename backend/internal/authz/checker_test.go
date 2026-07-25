package authz

import (
	"context"
	"errors"
	"sort"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

// The object-level checker is tested against a real Postgres for the same
// reason the membership methods are: every interesting part of it is a SQL
// predicate, a CHECK constraint or a view, and a mocked pgx would assert that
// the Go around them is unchanged while leaving the part that decides whether
// one tenant can read another's files completely unchecked.
//
// See testdb_test.go for how the throwaway database is created, and
// fixtures_test.go for the graph these assertions are made against.

// rebuilt runs the backfill and returns a checker. The migration's own backfill
// ran against an empty database (migrations are applied before any fixture
// exists), so every test that touches acl_object or acl_key starts here.
func rebuilt(t *testing.T) (*Checker, *world) {
	t.Helper()
	c, w := seed(t)
	if _, err := c.Rebuild(context.Background()); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	return c, w
}

func mustVerifyClean(t *testing.T, c *Checker) {
	t.Helper()
	report, err := c.Verify(context.Background(), 10)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if !report.Clean() {
		t.Errorf("%s", report.String())
		for _, s := range report.Samples {
			t.Errorf("  %s", s)
		}
	}
}

// ---------------------------------------------------------------------------
// Backfill
// ---------------------------------------------------------------------------

func TestRebuildIsRerunnableAndLeavesNoDrift(t *testing.T) {
	c, _ := seed(t)
	ctx := context.Background()

	if _, err := c.Rebuild(ctx); err != nil {
		t.Fatalf("first Rebuild: %v", err)
	}
	mustVerifyClean(t, c)

	// Re-runnable means the second pass is a no-op, not merely non-destructive.
	// A backfill that rewrites every row on every run would make the drift
	// verifier useless: nothing could ever be reported as stale.
	second, err := c.Rebuild(ctx)
	if err != nil {
		t.Fatalf("second Rebuild: %v", err)
	}
	if !second.Clean() {
		t.Errorf("second Rebuild changed rows: %+v", second)
	}
}

// TestBackfilledKeysMatchTheShippedSearchModel is the assertion that matters
// most in this file. internal/search already writes ACLs to Meilisearch, and
// acl_key has to agree with them byte for byte — a key that disagrees is either
// content nobody can find or content the wrong tenant can.
func TestBackfilledKeysMatchTheShippedSearchModel(t *testing.T) {
	c, w := rebuilt(t)

	tests := []struct {
		name string
		obj  ObjectRef
		want []string
	}{
		{
			"workspace: every member",
			WorkspaceObject(w.wsA),
			[]string{"w-" + w.wsA},
		},
		{
			// Both arms: the workspace key (public) and the member (channel admin).
			"public channel: the workspace plus its members",
			ChannelObject(w.chPublic),
			[]string{"u-" + w.owner, "w-" + w.wsA},
		},
		{
			"public channel with no members: the workspace alone",
			ChannelObject(w.chPublicEmpty),
			[]string{"w-" + w.wsA},
		},
		{
			// The one that must NOT carry the workspace key. If it did, every
			// member of wsA would hold a key to a private channel.
			"private channel: its members only",
			ChannelObject(w.chPrivate),
			[]string{"u-" + w.member},
		},
		{
			"dm: its participants only",
			ChannelObject(w.chDM),
			[]string{"u-" + w.admin, "u-" + w.owner},
		},
		{
			"another tenant's public channel carries only its own workspace key",
			ChannelObject(w.chOtherTenant),
			[]string{"w-" + w.wsB},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := storedKeys(t, c.pool, tt.obj)
			sort.Strings(tt.want)
			if !equalStrings(got, tt.want) {
				t.Errorf("acl_key(%s) = %v, want %v", tt.obj, got, tt.want)
			}
		})
	}
}

// TestBackfilledFileKeysMirrorCanRead pins the file arm: an attached file is
// readable by whoever may read its channel (one container key, not one key per
// member), and an unattached one only by its uploader.
func TestBackfilledFileKeysMirrorCanRead(t *testing.T) {
	c, w := seed(t)
	ctx := context.Background()

	attached := insertFile(t, c.pool, w.wsA, w.member, &w.msgInPrivate)
	unattached := insertFile(t, c.pool, w.wsA, w.owner, nil)
	if _, err := c.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}

	if got, want := storedKeys(t, c.pool, FileObject(attached)), []string{"c-" + w.chPrivate}; !equalStrings(got, want) {
		t.Errorf("attached file keys = %v, want %v", got, want)
	}
	if got, want := storedKeys(t, c.pool, FileObject(unattached)), []string{"u-" + w.owner}; !equalStrings(got, want) {
		t.Errorf("unattached file keys = %v, want %v", got, want)
	}

	// And the capability side agrees with file.Handler.canRead: workspace
	// membership is deliberately not enough for a file in a private channel.
	assertCan(t, c, UserSubject(w.member), FileObject(attached), CapRead, true)
	assertCan(t, c, UserSubject(w.admin), FileObject(attached), CapRead, false)
	assertCan(t, c, UserSubject(w.owner), FileObject(unattached), CapRead, true)
	assertCan(t, c, UserSubject(w.member), FileObject(unattached), CapRead, false)
	assertCan(t, c, UserSubject(w.stranger), FileObject(attached), CapRead, false)

	mustVerifyClean(t, c)
}

// ---------------------------------------------------------------------------
// Capability against the predicates it must reproduce
// ---------------------------------------------------------------------------

// TestCapabilityReproducesChannelPredicates walks the whole fixture matrix. It
// is the unit-test half of the dual-run comparison: whatever CanReadChannel and
// IsChannelMember answer, Capability must answer the same, for every pair.
func TestCapabilityReproducesChannelPredicates(t *testing.T) {
	c, w := seed(t)
	ctx := context.Background()

	users := map[string]string{
		"owner": w.owner, "admin": w.admin, "member": w.member, "guest": w.guest,
		"stranger": w.stranger, "multiAdmin": w.multiAdmin,
	}
	channels := map[string]string{
		"public": w.chPublic, "publicEmpty": w.chPublicEmpty, "private": w.chPrivate,
		"dm": w.chDM, "archived": w.chArchived, "otherTenant": w.chOtherTenant,
		"missing": missingID,
	}

	for uName, userID := range users {
		for chName, channelID := range channels {
			t.Run(uName+"/"+chName, func(t *testing.T) {
				info, err := c.Channel(ctx, channelID)
				if err != nil {
					t.Fatalf("Channel: %v", err)
				}
				legacyRead, err := c.CanReadChannel(ctx, info, userID)
				if err != nil {
					t.Fatalf("CanReadChannel: %v", err)
				}
				legacyMember, err := c.IsChannelMember(ctx, channelID, userID)
				if err != nil {
					t.Fatalf("IsChannelMember: %v", err)
				}

				assertCan(t, c, UserSubject(userID), ChannelObject(channelID), CapRead, legacyRead)
				assertCan(t, c, UserSubject(userID), ChannelObject(channelID), CapWrite, legacyMember)
			})
		}
	}
}

func TestCapabilityReproducesWorkspaceRoles(t *testing.T) {
	c, w := seed(t)
	ctx := context.Background()

	tests := []struct {
		user string
		want Capability
	}{
		{w.owner, CapAdmin},
		{w.admin, CapAdmin},
		{w.member, CapWrite},
		{w.guest, CapRead},
		{w.stranger, CapNone},
	}
	for _, tt := range tests {
		got, err := c.Capability(ctx, UserSubject(tt.user), WorkspaceObject(w.wsA))
		if err != nil {
			t.Fatalf("Capability: %v", err)
		}
		if got != tt.want {
			t.Errorf("Capability(%s, wsA) = %s, want %s", tt.user, got, tt.want)
		}
	}
}

// TestCapabilityDistinguishesMissingFromDenied is the err/!ok rule, which this
// package exists to keep un-collapsed: a missing object and an unparseable id
// are denials, not errors.
func TestCapabilityDistinguishesMissingFromDenied(t *testing.T) {
	c, w := seed(t)
	ctx := context.Background()

	for _, obj := range []ObjectRef{
		ChannelObject(missingID),
		WorkspaceObject(missingID),
		ChannelObject("not-a-uuid"),
		ChannelObject(""),
	} {
		got, err := c.Capability(ctx, UserSubject(w.owner), obj)
		if err != nil {
			t.Errorf("Capability(%s) returned an error (%v); a missing object is a 404, not a 500", obj, err)
		}
		if got != CapNone {
			t.Errorf("Capability(%s) = %s, want none", obj, got)
		}
	}

	// A malformed object TYPE is a different thing: it can only come from code,
	// so it is a bug and says so.
	if _, err := c.Capability(ctx, UserSubject(w.owner), ObjectRef{Type: "Not A Type", ID: missingID}); err == nil {
		t.Error("a malformed object type must be an error, not a silent denial")
	}
}

// ---------------------------------------------------------------------------
// The list path
// ---------------------------------------------------------------------------

func TestKeysForMatchesTheSearchKeySet(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()

	tests := []struct {
		name string
		user string
		want []string
	}{
		{
			// A plain member: the workspace, themselves, every public channel,
			// and the one private channel they belong to.
			"member",
			w.member,
			[]string{
				"c-" + w.chArchived, "c-" + w.chPrivate, "c-" + w.chPublic, "c-" + w.chPublicEmpty,
				"u-" + w.member, "w-" + w.wsA,
			},
		},
		{
			// The guest is in no channel, so only the public ones.
			"guest",
			w.guest,
			[]string{
				"c-" + w.chArchived, "c-" + w.chPublic, "c-" + w.chPublicEmpty,
				"u-" + w.guest, "w-" + w.wsA,
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.KeysFor(ctx, UserSubject(tt.user), w.wsA)
			if err != nil {
				t.Fatalf("KeysFor: %v", err)
			}
			sort.Strings(tt.want)
			if !equalStrings(got, tt.want) {
				t.Errorf("KeysFor = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestKeysForNonMemberIsEmpty is the tenancy assertion. An empty key set has to
// be rendered by every caller as "no results" rather than "no filter" — a
// dropped narrowing term widens the query, and a widened tenancy filter is a
// cross-tenant leak.
func TestKeysForNonMemberIsEmpty(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()

	for _, tt := range []struct{ name, user, ws string }{
		{"stranger in wsA", w.stranger, w.wsA},
		{"member in wsB", w.member, w.wsB},
		{"unknown workspace", w.owner, missingID},
		{"unparseable workspace", w.owner, "not-a-uuid"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got, err := c.KeysFor(ctx, UserSubject(tt.user), tt.ws)
			if err != nil {
				t.Fatalf("KeysFor: %v", err)
			}
			if len(got) != 0 {
				t.Errorf("KeysFor = %v, want an empty set", got)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Grants, inheritance and the key rewrite
// ---------------------------------------------------------------------------

// TestGrantInheritsThroughTheSubtree exercises the whole ACL-native path:
// register a folder tree, share the top of it, and assert that (a) the
// capability is inherited, (b) acl_key was rewritten in the same transaction,
// (c) the descendant carries the BOUNDED container key rather than one key per
// person, and (d) the materialization still matches its definition afterwards.
func TestGrantInheritsThroughTheSubtree(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	parent := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	child := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	doc := ObjectRef{Type: "doc", ID: uuid.NewString()}

	mustRegister(t, c, parent, WorkspaceObject(w.wsA))
	mustRegister(t, c, child, parent)
	mustRegister(t, c, doc, child)

	// Before the grant: nobody but a grantee can reach an ACL-native object.
	// Absence is denial — there is no negative grant to write.
	assertCan(t, c, UserSubject(w.member), doc, CapRead, false)
	assertCan(t, c, UserSubject(w.owner), doc, CapRead, false)

	if err := c.Grant(ctx, actor, UserSubject(w.member), parent, CapComment); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Revoke(context.Background(), actor, UserSubject(w.member), parent); err != nil {
			t.Errorf("cleanup Revoke: %v", err)
		}
	})

	// Inherited, and at the granted level — not silently upgraded.
	assertCapability(t, c, UserSubject(w.member), parent, CapComment)
	assertCapability(t, c, UserSubject(w.member), child, CapComment)
	assertCapability(t, c, UserSubject(w.member), doc, CapComment)
	assertCan(t, c, UserSubject(w.member), doc, CapWrite, false)
	assertCan(t, c, UserSubject(w.guest), doc, CapRead, false)

	// The key rewrite happened in the same transaction as the grant, and the
	// descendant carries its container's key — one row, however many people the
	// folder is shared with.
	if got, want := storedKeys(t, c.pool, parent), []string{"u-" + w.member}; !equalStrings(got, want) {
		t.Errorf("parent folder keys = %v, want %v", got, want)
	}
	if got, want := storedKeys(t, c.pool, doc), []string{"f-" + child.ID, "u-" + w.member}; !equalStrings(got, want) {
		t.Errorf("doc keys = %v, want %v", got, want)
	}

	// And the list path sees it: the folder keys appear in the caller's key set,
	// which is what a search filter or a paged listing intersects against.
	keys, err := c.KeysFor(ctx, UserSubject(w.member), w.wsA)
	if err != nil {
		t.Fatalf("KeysFor: %v", err)
	}
	if !contains(keys, "f-"+parent.ID) || !contains(keys, "f-"+child.ID) {
		t.Errorf("KeysFor = %v, want it to contain f-%s and f-%s", keys, parent.ID, child.ID)
	}

	readable, err := c.FilterReadable(ctx, keys, "doc", []string{doc.ID, uuid.NewString()})
	if err != nil {
		t.Fatalf("FilterReadable: %v", err)
	}
	if len(readable) != 1 || readable[0] != doc.ID {
		t.Errorf("FilterReadable = %v, want [%s]", readable, doc.ID)
	}

	// Someone with no grant filters down to nothing rather than to everything.
	strangerKeys, err := c.KeysFor(ctx, UserSubject(w.guest), w.wsA)
	if err != nil {
		t.Fatalf("KeysFor(guest): %v", err)
	}
	denied, err := c.FilterReadable(ctx, strangerKeys, "doc", []string{doc.ID})
	if err != nil {
		t.Fatalf("FilterReadable(guest): %v", err)
	}
	if len(denied) != 0 {
		t.Errorf("FilterReadable(guest) = %v, want empty", denied)
	}

	mustVerifyClean(t, c)
}

func TestRevokeRemovesInheritedKeysImmediately(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	folder := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	doc := ObjectRef{Type: "doc", ID: uuid.NewString()}
	mustRegister(t, c, folder, WorkspaceObject(w.wsA))
	mustRegister(t, c, doc, folder)

	if err := c.Grant(ctx, actor, UserSubject(w.member), folder, CapWrite); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	assertCan(t, c, UserSubject(w.member), doc, CapRead, true)

	if err := c.Revoke(ctx, actor, UserSubject(w.member), folder); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	assertCan(t, c, UserSubject(w.member), doc, CapRead, false)
	if got := storedKeys(t, c.pool, folder); len(got) != 0 {
		t.Errorf("folder keys after revoke = %v, want none", got)
	}

	// Revoking again is not an error: the caller asked for a state that already
	// holds.
	if err := c.Revoke(ctx, actor, UserSubject(w.member), folder); err != nil {
		t.Errorf("second Revoke: %v", err)
	}
	mustVerifyClean(t, c)
}

func TestMoveRewritesSubtreePathsAndKeys(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	shared := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	other := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	moving := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	doc := ObjectRef{Type: "doc", ID: uuid.NewString()}

	mustRegister(t, c, shared, WorkspaceObject(w.wsA))
	mustRegister(t, c, other, WorkspaceObject(w.wsA))
	mustRegister(t, c, moving, shared)
	mustRegister(t, c, doc, moving)

	if err := c.Grant(ctx, actor, UserSubject(w.member), shared, CapWrite); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Revoke(context.Background(), actor, UserSubject(w.member), shared); err != nil {
			t.Errorf("cleanup Revoke: %v", err)
		}
	})
	assertCan(t, c, UserSubject(w.member), doc, CapRead, true)

	// Moving the subtree out of the shared folder must revoke the inherited
	// access for the whole subtree, not just for the object that moved.
	if err := c.Move(ctx, actor, moving, other); err != nil {
		t.Fatalf("Move: %v", err)
	}

	if got, want := storedPath(t, c.pool, doc),
		"/workspace:"+w.wsA+"/folder:"+other.ID+"/folder:"+moving.ID+"/doc:"+doc.ID+"/"; got != want {
		t.Errorf("doc path after move = %q, want %q", got, want)
	}
	assertCan(t, c, UserSubject(w.member), moving, CapRead, false)
	assertCan(t, c, UserSubject(w.member), doc, CapRead, false)
	if got, want := storedKeys(t, c.pool, doc), []string{"f-" + moving.ID}; !equalStrings(got, want) {
		t.Errorf("doc keys after move = %v, want %v", got, want)
	}

	mustVerifyClean(t, c)
}

func TestMoveRefusals(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	inA := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	childOfA := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	inB := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	mustRegister(t, c, inA, WorkspaceObject(w.wsA))
	mustRegister(t, c, childOfA, inA)
	mustRegister(t, c, inB, WorkspaceObject(w.wsB))

	t.Run("across workspaces", func(t *testing.T) {
		// The one that would be a tenancy breach: acl_object.workspace_id stays
		// authoritative, so a move can never relocate an object into another
		// tenant.
		if err := c.Move(ctx, actor, inA, inB); err == nil {
			t.Fatal("Move across workspaces succeeded")
		}
	})
	t.Run("into its own subtree", func(t *testing.T) {
		if err := c.Move(ctx, actor, inA, childOfA); err == nil {
			t.Fatal("Move into a descendant succeeded; that is a cycle")
		}
	})
	t.Run("a derived type", func(t *testing.T) {
		err := c.Move(ctx, actor, ChannelObject(w.chPublic), inA)
		if !errors.Is(err, ErrDerivedType) {
			t.Fatalf("Move(channel) = %v, want ErrDerivedType", err)
		}
	})
	t.Run("an unregistered object", func(t *testing.T) {
		err := c.Move(ctx, actor, ObjectRef{Type: TypeFolder, ID: uuid.NewString()}, inA)
		if !errors.Is(err, ErrNotRegistered) {
			t.Fatalf("Move(unregistered) = %v, want ErrNotRegistered", err)
		}
	})
	mustVerifyClean(t, c)
}

// TestRegisterRefusesPastTheDepthCap: a path is text, and an unbounded nesting
// depth makes every subtree predicate longer and every move more expensive. The
// cap is enforced in Go and again by acl_object's CHECK.
func TestRegisterRefusesPastTheDepthCap(t *testing.T) {
	c, w := rebuilt(t)

	parent := WorkspaceObject(w.wsA)
	for depth := 2; depth <= MaxPathDepth; depth++ {
		next := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
		if err := c.Register(context.Background(), next, parent); err != nil {
			t.Fatalf("Register at depth %d: %v", depth, err)
		}
		parent = next
	}
	tooDeep := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	if err := c.Register(context.Background(), tooDeep, parent); err == nil {
		t.Fatalf("Register at depth %d succeeded, want a refusal past %d", MaxPathDepth+1, MaxPathDepth)
	}
	mustVerifyClean(t, c)
}

func TestRegisterRefusesDerivedTypes(t *testing.T) {
	c, w := rebuilt(t)
	err := c.Register(context.Background(), ChannelObject(uuid.NewString()), WorkspaceObject(w.wsA))
	if !errors.Is(err, ErrDerivedType) {
		t.Fatalf("Register(channel) = %v, want ErrDerivedType", err)
	}
}

// ---------------------------------------------------------------------------
// Audit
// ---------------------------------------------------------------------------

func TestGrantsForAndSubjectsOf(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()
	actor := UserSubject(w.owner)

	folder := ObjectRef{Type: TypeFolder, ID: uuid.NewString()}
	doc := ObjectRef{Type: "doc", ID: uuid.NewString()}
	mustRegister(t, c, folder, WorkspaceObject(w.wsA))
	mustRegister(t, c, doc, folder)

	if err := c.Grant(ctx, actor, UserSubject(w.member), folder, CapWrite); err != nil {
		t.Fatalf("Grant folder: %v", err)
	}
	if err := c.Grant(ctx, actor, UserSubject(w.guest), doc, CapRead); err != nil {
		t.Fatalf("Grant doc: %v", err)
	}
	t.Cleanup(func() {
		ctx := context.Background()
		if err := c.Revoke(ctx, actor, UserSubject(w.member), folder); err != nil {
			t.Errorf("cleanup Revoke: %v", err)
		}
		if err := c.Revoke(ctx, actor, UserSubject(w.guest), doc); err != nil {
			t.Errorf("cleanup Revoke: %v", err)
		}
	})

	// "What did this person have access to."
	grants, err := c.GrantsFor(ctx, UserSubject(w.member))
	if err != nil {
		t.Fatalf("GrantsFor: %v", err)
	}
	if len(grants) != 1 || grants[0].Object != folder || grants[0].Capability != CapWrite {
		t.Fatalf("GrantsFor = %+v, want one write grant on %s", grants, folder)
	}
	if grants[0].WorkspaceID != w.wsA || grants[0].GrantedBy != w.owner {
		t.Errorf("GrantsFor lost its provenance: %+v", grants[0])
	}

	// "Who can see this." Inherited grants count: a share on the parent folder
	// is just as much an answer as a share on the document.
	subjects, err := c.SubjectsOf(ctx, doc)
	if err != nil {
		t.Fatalf("SubjectsOf: %v", err)
	}
	if len(subjects) != 2 {
		t.Fatalf("SubjectsOf = %+v, want the direct grant and the inherited one", subjects)
	}
	// Ordered root-first, so the caller can tell where each grant lives.
	if subjects[0].Object != folder || subjects[1].Object != doc {
		t.Errorf("SubjectsOf order = %s then %s, want %s then %s",
			subjects[0].Object, subjects[1].Object, folder, doc)
	}
}

// ---------------------------------------------------------------------------
// Drift
// ---------------------------------------------------------------------------

// TestVerifyReportsInjectedDrift proves the verifier can actually see the two
// failures that matter, and names them precisely enough to debug. A verifier
// that only ever reports "clean" is indistinguishable from one that is broken.
func TestVerifyReportsInjectedDrift(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()

	// An extra key is a LEAK: somebody holds a key to an object nothing granted
	// them.
	if _, err := c.pool.Exec(ctx,
		`INSERT INTO acl_key (object_type, object_id, key) VALUES ('channel', $1, $2)`,
		w.chPrivate, "u-"+w.stranger); err != nil {
		t.Fatalf("inject extra key: %v", err)
	}
	// A missing key is an OUTAGE: somebody cannot see something they should.
	if _, err := c.pool.Exec(ctx,
		`DELETE FROM acl_key WHERE object_type = 'workspace' AND object_id = $1`, w.wsB); err != nil {
		t.Fatalf("delete key: %v", err)
	}

	report, err := c.Verify(ctx, 10)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.ExtraKeys != 1 || report.MissingKeys != 1 {
		t.Errorf("Verify = %s, want exactly one extra and one missing key", report.String())
	}
	if !hasSample(report, "extra_key", "u-"+w.stranger) {
		t.Errorf("no extra_key sample named the injected key: %+v", report.Samples)
	}
	if !hasSample(report, "missing_key", "w-"+w.wsB) {
		t.Errorf("no missing_key sample named the deleted key: %+v", report.Samples)
	}

	// Verify never repairs — a repair hides the bug that caused the drift.
	if got := storedKeys(t, c.pool, ChannelObject(w.chPrivate)); !contains(got, "u-"+w.stranger) {
		t.Error("Verify repaired the injected key; it must report, not repair")
	}

	// Rebuild is the thing that repairs, explicitly and on request.
	if _, err := c.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	mustVerifyClean(t, c)
}

func TestVerifyReportsUnregisteredObjects(t *testing.T) {
	c, w := rebuilt(t)
	ctx := context.Background()

	// Nothing registers an object at creation time until the cutover, so a
	// channel created after the last Rebuild is genuinely missing from the
	// materialization — and the verifier has to say so rather than call it
	// clean.
	fresh := insertChannel(t, c.pool, w.wsA)
	report, err := c.Verify(ctx, 10)
	if err != nil {
		t.Fatalf("Verify: %v", err)
	}
	if report.MissingObjects != 1 || !hasSample(report, "missing_object", fresh) {
		t.Errorf("Verify = %s, want one missing object naming %s: %+v", report.String(), fresh, report.Samples)
	}

	if _, err := c.Rebuild(ctx); err != nil {
		t.Fatalf("Rebuild: %v", err)
	}
	mustVerifyClean(t, c)
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func mustRegister(t *testing.T, c *Checker, obj, parent ObjectRef) {
	t.Helper()
	if err := c.Register(context.Background(), obj, parent); err != nil {
		t.Fatalf("Register(%s under %s): %v", obj, parent, err)
	}
}

func assertCan(t *testing.T, c *Checker, sub SubjectRef, obj ObjectRef, want Capability, expect bool) {
	t.Helper()
	got, err := c.Can(context.Background(), sub, obj, want)
	if err != nil {
		t.Fatalf("Can(%s, %s, %s): %v", sub, obj, want, err)
	}
	if got != expect {
		t.Errorf("Can(%s, %s, %s) = %t, want %t", sub, obj, want, got, expect)
	}
}

func assertCapability(t *testing.T, c *Checker, sub SubjectRef, obj ObjectRef, want Capability) {
	t.Helper()
	got, err := c.Capability(context.Background(), sub, obj)
	if err != nil {
		t.Fatalf("Capability(%s, %s): %v", sub, obj, err)
	}
	if got != want {
		t.Errorf("Capability(%s, %s) = %s, want %s", sub, obj, got, want)
	}
}

func storedKeys(t *testing.T, pool *pgxpool.Pool, obj ObjectRef) []string {
	t.Helper()
	rows, err := pool.Query(context.Background(),
		`SELECT key FROM acl_key WHERE object_type = $1 AND object_id = $2 ORDER BY key`,
		obj.Type, obj.ID)
	if err != nil {
		t.Fatalf("read acl_key: %v", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var k string
		if err := rows.Scan(&k); err != nil {
			t.Fatalf("scan acl_key: %v", err)
		}
		out = append(out, k)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("read acl_key: %v", err)
	}
	return out
}

func storedPath(t *testing.T, pool *pgxpool.Pool, obj ObjectRef) string {
	t.Helper()
	var path string
	if err := pool.QueryRow(context.Background(),
		`SELECT path FROM acl_object WHERE object_type = $1 AND object_id = $2`,
		obj.Type, obj.ID).Scan(&path); err != nil {
		t.Fatalf("read acl_object path: %v", err)
	}
	return path
}

func insertFile(t *testing.T, pool *pgxpool.Pool, workspaceID, userID string, messageID *string) string {
	t.Helper()
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO files (workspace_id, user_id, message_id, name, content_type, size_bytes, storage_key)
		VALUES ($1, $2, $3, 'f.txt', 'text/plain', 1, $4) RETURNING id`,
		workspaceID, userID, messageID, uuid.NewString()).Scan(&id); err != nil {
		t.Fatalf("insert file: %v", err)
	}
	return id
}

func insertChannel(t *testing.T, pool *pgxpool.Pool, workspaceID string) string {
	t.Helper()
	slug := "fresh-" + uuid.NewString()[:8]
	var id string
	if err := pool.QueryRow(context.Background(), `
		INSERT INTO channels (workspace_id, name, slug, type)
		VALUES ($1, $2, $2, 'public'::channel_type) RETURNING id`,
		workspaceID, slug).Scan(&id); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	return id
}

func hasSample(r DriftReport, kind, needle string) bool {
	for _, s := range r.Samples {
		if s.Kind == kind && (strings.Contains(s.Detail, needle) || s.ObjectID == needle) {
			return true
		}
	}
	return false
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func contains(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}
