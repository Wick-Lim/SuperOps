package authz

import (
	"strings"
	"testing"
)

// These tests need no database: they are about the encodings that everything
// else in the package assumes, and getting one of them wrong is not a wrong
// answer, it is a key that fails validation and disappears from a filter.

func TestCapabilityOrdering(t *testing.T) {
	ladder := []Capability{CapRead, CapComment, CapWrite, CapShare, CapAdmin}
	for i, lower := range ladder {
		for j, higher := range ladder {
			want := j >= i
			if got := higher.Implies(lower); got != want {
				t.Errorf("%s.Implies(%s) = %t, want %t", higher, lower, got, want)
			}
		}
	}
}

func TestCapabilityNoneImpliesNothing(t *testing.T) {
	// The zero value must deny. A Capability that was never assigned is the
	// most likely one to reach a check by accident.
	for _, want := range []Capability{CapNone, CapRead, CapAdmin} {
		if CapNone.Implies(want) {
			t.Errorf("CapNone.Implies(%s) = true, want false", want)
		}
	}
	// And "wants nothing" must not be satisfiable either, or a caller that
	// forgot to state a requirement would be granted one.
	if CapAdmin.Implies(CapNone) {
		t.Error("CapAdmin.Implies(CapNone) = true, want false")
	}
}

func TestParseCapability(t *testing.T) {
	for name, want := range map[string]Capability{
		"read": CapRead, "COMMENT": CapComment, " write ": CapWrite,
		"share": CapShare, "admin": CapAdmin,
	} {
		got, ok := ParseCapability(name)
		if !ok || got != want {
			t.Errorf("ParseCapability(%q) = %s,%t; want %s,true", name, got, ok, want)
		}
	}
	for _, bad := range []string{"", "none", "owner", "read-write", "admin;"} {
		if got, ok := ParseCapability(bad); ok {
			t.Errorf("ParseCapability(%q) = %s,true; want ok=false", bad, got)
		}
	}
}

func TestCapabilityStorableRefusesNone(t *testing.T) {
	if _, err := CapNone.storable(); err == nil {
		t.Error("CapNone.storable() succeeded; a stored 'no access' grant is the negative grant this model excludes")
	}
	if got, err := CapShare.storable(); err != nil || got != "share" {
		t.Errorf("CapShare.storable() = %q,%v; want \"share\",nil", got, err)
	}
}

const (
	testUUID  = "11111111-1111-4111-8111-111111111111"
	testUUID2 = "22222222-2222-4222-8222-222222222222"
)

// TestKeyEncoding pins the format internal/search's validKey accepts. It is
// duplicated there deliberately (TestAuthzKeysPassSearchValidation) — this side
// asserts what is produced, that side asserts what is accepted, and the two
// packages must not be able to drift apart silently.
func TestKeyEncoding(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"workspace", WorkspaceKey(testUUID), "w-" + testUUID},
		{"channel container", ContainerKey(ChannelObject(testUUID)), "c-" + testUUID},
		{"folder container", ContainerKey(ObjectRef{Type: TypeFolder, ID: testUUID}), "f-" + testUUID},
		{"user subject", UserSubject(testUUID).Key(), "u-" + testUUID},
		{"group subject", GroupSubject(testUUID).Key(), "g-" + testUUID},
		{"uppercase is canonicalised", ContainerKey(ChannelObject(strings.ToUpper(testUUID))), "c-" + testUUID},
		{"a file is not a container", ContainerKey(FileObject(testUUID)), ""},
		{"a workspace is not a container", ContainerKey(WorkspaceObject(testUUID)), ""},
		{"non-uuid yields no key", ContainerKey(ChannelObject("general")), ""},
		{"injection attempt yields no key", WorkspaceKey(`" OR "1"="1`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("key = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestObjectTypeAlphabetExcludesLikeMetacharacters(t *testing.T) {
	// A '_' in a type name would make every `path LIKE prefix || '%'` subtree
	// predicate a single-character wildcard — a subtree that quietly matches its
	// siblings. Excluding it from the alphabet is the whole reason no call site
	// escapes a path.
	for _, bad := range []string{"my_type", "Folder", "", "1folder", "folder%", "folder/x", strings.Repeat("f", 33)} {
		if err := validObjectType(bad); err == nil {
			t.Errorf("validObjectType(%q) = nil, want an error", bad)
		}
	}
	for _, good := range []string{"folder", "doc", "sheet", "design", "issue", "project", "mailbox", "c3"} {
		if err := validObjectType(good); err != nil {
			t.Errorf("validObjectType(%q) = %v, want nil", good, err)
		}
	}
}

func TestPathHelpers(t *testing.T) {
	ws := "/workspace:" + testUUID + "/"
	ch := buildPath(ws, ChannelObject(testUUID2))

	if want := ws + "channel:" + testUUID2 + "/"; ch != want {
		t.Fatalf("buildPath = %q, want %q", ch, want)
	}
	if got := pathDepth(ws); got != 1 {
		t.Errorf("pathDepth(workspace) = %d, want 1", got)
	}
	if got := pathDepth(ch); got != 2 {
		t.Errorf("pathDepth(channel) = %d, want 2", got)
	}
	if !hasPrefixPath(ch, ws) {
		t.Error("a channel path must be inside its workspace path")
	}
	if hasPrefixPath(ws, ch) {
		t.Error("a workspace path must not be inside its channel's path")
	}
}

// TestPathPrefixIsSegmentAligned is the reason every stored path ends in '/'.
// Without the trailing separator '/workspace:a/folder:1' is a prefix of
// '/workspace:a/folder:10', and a subtree rewrite would take a sibling's
// children with it.
func TestPathPrefixIsSegmentAligned(t *testing.T) {
	a := "/workspace:" + testUUID + "/folder:" + testUUID2 + "/"
	sibling := "/workspace:" + testUUID + "/folder:" + testUUID2 + "x/"
	if hasPrefixPath(sibling, a) {
		t.Error("a sibling whose id extends another's must not look like a descendant")
	}
}

func TestAncestorPaths(t *testing.T) {
	ws := "/workspace:" + testUUID + "/"
	ch := ws + "channel:" + testUUID2 + "/"
	file := ch + "file:" + testUUID + "/"

	got := ancestorPaths(file)
	want := []string{ws, ch, file}
	if len(got) != len(want) {
		t.Fatalf("ancestorPaths = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("ancestorPaths[%d] = %q, want %q", i, got[i], want[i])
		}
	}

	// Past the cap it returns nothing rather than a truncated list: truncating
	// would silently drop the grants nearest the root, which is a denial that
	// looks like a policy decision.
	deep := "/"
	for i := 0; i <= MaxPathDepth; i++ {
		deep += "folder:" + testUUID + "/"
	}
	if got := ancestorPaths(deep); got != nil {
		t.Errorf("ancestorPaths past the depth cap returned %d entries, want none", len(got))
	}
}

func TestNormalizeRejectsBadTypeButNotBadID(t *testing.T) {
	// A malformed TYPE is a compiled constant gone wrong — a bug, and an error.
	if _, _, err := (ObjectRef{Type: "Bad_Type", ID: testUUID}).normalize(); err == nil {
		t.Error("a malformed object type must be an error")
	}
	// A malformed ID came from a URL. It names no object, which is a 404, and
	// turning it into a 500 would make every fuzzer look like an outage.
	ref, ok, err := (ObjectRef{Type: TypeChannel, ID: "not-a-uuid"}).normalize()
	if err != nil || ok {
		t.Errorf("normalize(bad id) = %v,%t,%v; want zero,false,nil", ref, ok, err)
	}
}
