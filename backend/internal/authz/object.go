package authz

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

// ---------------------------------------------------------------------------
// Capability
// ---------------------------------------------------------------------------

// Capability is how much a subject may do to an object. The values are ordered
// and each implies every value below it:
//
//	admin > share > write > comment > read
//
// Ordering is the point. It makes a check a comparison rather than a set
// membership test, and it means a capability inserted between two existing ones
// does not touch a single call site. It also makes "effective capability" a
// max() over the grants that apply, which is why deny is absence rather than a
// negative grant — see docs/plans/00-permissions.md.
type Capability uint8

const (
	// CapNone is the zero value and means no access at all. It is deliberately
	// the zero value: a Capability that was never assigned denies.
	CapNone Capability = iota
	CapRead
	CapComment
	CapWrite
	CapShare
	CapAdmin
)

var capabilityNames = map[Capability]string{
	CapNone:    "none",
	CapRead:    "read",
	CapComment: "comment",
	CapWrite:   "write",
	CapShare:   "share",
	CapAdmin:   "admin",
}

// capabilityValues is the parse direction, and its keys are exactly the values
// acl_grant.capability's CHECK constraint accepts. "none" is absent on purpose:
// a stored grant of "no access" is the negative grant this model excludes.
var capabilityValues = map[string]Capability{
	"read":    CapRead,
	"comment": CapComment,
	"write":   CapWrite,
	"share":   CapShare,
	"admin":   CapAdmin,
}

func (c Capability) String() string {
	if s, ok := capabilityNames[c]; ok {
		return s
	}
	return fmt.Sprintf("capability(%d)", uint8(c))
}

// Implies reports whether holding c is enough for want. CapNone implies nothing,
// including itself: "may they read" asked of a subject with no access is false,
// not true-because-they-want-nothing.
func (c Capability) Implies(want Capability) bool {
	if c == CapNone || want == CapNone {
		return false
	}
	return c >= want
}

// ParseCapability turns a stored or caller-supplied string into a Capability.
// Unknown input is rejected rather than defaulting: defaulting to read grants
// access on a typo, and defaulting to none denies it silently.
func ParseCapability(s string) (Capability, bool) {
	c, ok := capabilityValues[strings.ToLower(strings.TrimSpace(s))]
	return c, ok
}

// storable renders a capability for acl_grant.capability, refusing CapNone.
func (c Capability) storable() (string, error) {
	name, ok := capabilityNames[c]
	if !ok || c == CapNone {
		return "", fmt.Errorf("capability %s cannot be granted", c)
	}
	return name, nil
}

// ---------------------------------------------------------------------------
// Object and subject references
// ---------------------------------------------------------------------------

// Object types. The set is open — the ACL layer must never enumerate the types
// the product has, or every new pillar is a change here — but the three below
// are special: their hierarchy and their keys are DERIVED from the pre-ACL
// schema rather than owned by acl_object. See derivedTypes.
const (
	TypeWorkspace = "workspace"
	TypeChannel   = "channel"
	TypeFile      = "file"
	// TypeFolder does not exist until Drive (ROADMAP phase 1). It is named here
	// because the path helpers and the f- key prefix already understand it, so
	// Drive adds rows rather than concepts.
	TypeFolder = "folder"
	// TypeDoc names a collaborative document (internal/collab). Nothing
	// registers one yet — a document has no place in the hierarchy until Drive
	// gives it a folder to hang off — but the type is named here because it is
	// the second object type with a LIVE surface, and liveTypes below has to
	// enumerate those.
	TypeDoc = "doc"
)

// liveTypes are the object types a subject can hold an open, already-authorized
// connection to: a WebSocket subscription to a channel, an editing session in a
// document. They are the types where withdrawing a grant is not finished when
// the row is deleted — see Revoker.
//
// A file is absent on purpose. There is no such thing as an open file session;
// the next download re-authorizes.
var liveTypes = []string{TypeChannel, TypeDoc}

// derivedTypes are the object types whose acl_object row and whose acl_key rows
// are computed from workspaces/channels/files/channel_members by the
// acl_object_expected and acl_key_expected views, and reconciled by Rebuild.
//
// They may not be registered or moved through this API: the legacy tables are
// still authoritative for them, so an acl_object row that disagrees would be
// silently reverted by the next Rebuild and reported as drift in between.
var derivedTypes = map[string]bool{
	TypeWorkspace: true,
	TypeChannel:   true,
	TypeFile:      true,
}

// containerKeyPrefix maps a container object type to the access-key prefix that
// grants read on its contents. A type absent from this map is not a container:
// nothing inherits a key from it.
//
// A workspace is deliberately not here. 'w-' means "member of the workspace",
// which is not "may read everything in it" — conflating the two is exactly how
// a private channel becomes workspace-readable.
var containerKeyPrefix = map[string]string{
	TypeChannel: "c-",
	TypeFolder:  "f-",
}

// Subject types. Mirrors acl_grant.subject_type's CHECK constraint.
const (
	SubjectUser  = "user"
	SubjectGroup = "group" // reserved; matches the g- key prefix, nothing writes it yet
	// SubjectWorkspace is "everyone in this workspace". It is what makes Drive
	// the company's shared drive without inventing a second inheritance
	// mechanism: one acl_grant row on the Drive root, and the ordinary subtree
	// inheritance carries it to every folder and file below.
	//
	// Its key is w-<workspace>, which KeysFor already puts in every member's key
	// set, so nothing downstream had to learn a new prefix. What it is NOT is a
	// tenancy statement: acl_object.workspace_id decides that, and a workspace
	// grant on an object in another tenant would still fail the path CHECK.
	SubjectWorkspace = "workspace"

	// SubjectLink is a share link: one anonymous holder of one token, granted
	// on one object.
	//
	// Its key prefix is 'g-' rather than a sixth prefix, and that is deliberate.
	// internal/search's validKey accepts a CLOSED set and a key that fails it is
	// DROPPED from the filter — and a dropped narrowing term WIDENS the query,
	// which for a tenancy filter is a cross-tenant leak. Widening that validator
	// in order to ship a link is exactly the change docs/plans/README.md ruling
	// 3 exists to prevent, and 'g-' already means "a subject that is not one
	// person".
	SubjectLink = "link"
)

var subjectKeyPrefix = map[string]string{
	SubjectUser:      "u-",
	SubjectGroup:     "g-",
	SubjectWorkspace: "w-",
	SubjectLink:      "g-",
}

// ObjectRef names one object: a type and a uuid. It is a value, not an
// interface, because every consumer of this package builds one from two strings
// it already has.
type ObjectRef struct {
	Type string
	ID   string
}

// SubjectRef names who is asking. A user today, a group later, a share-link
// token eventually — which is why nothing in this package takes a bare user id.
type SubjectRef struct {
	Type string
	ID   string
}

// UserSubject is the constructor every call site uses today.
func UserSubject(userID string) SubjectRef { return SubjectRef{Type: SubjectUser, ID: userID} }

// GroupSubject is reserved for when authz grows groups. Declared so the
// subject-type namespace is decided in one place.
func GroupSubject(groupID string) SubjectRef { return SubjectRef{Type: SubjectGroup, ID: groupID} }

// WorkspaceSubject names every member of a workspace at once. Granting to it is
// how an object becomes workspace-shared; see SubjectWorkspace.
func WorkspaceSubject(workspaceID string) SubjectRef {
	return SubjectRef{Type: SubjectWorkspace, ID: workspaceID}
}

// LinkSubject names one share link. Its key is what a resolved link session
// holds, and it is the ONLY key such a session holds.
func LinkSubject(linkID string) SubjectRef {
	return SubjectRef{Type: SubjectLink, ID: linkID}
}

// Ref constructors for the types that exist today.
//
// FolderObject is the odd one out and worth the note: workspaces, channels and
// files are DERIVED (their rows come from acl_object_expected), so their refs
// are only ever read. A folder is ACL-native — Register writes its row and Move
// rewrites its subtree — so this is the one constructor whose result is passed
// to a mutation.
func WorkspaceObject(id string) ObjectRef { return ObjectRef{Type: TypeWorkspace, ID: id} }
func ChannelObject(id string) ObjectRef   { return ObjectRef{Type: TypeChannel, ID: id} }
func FileObject(id string) ObjectRef      { return ObjectRef{Type: TypeFile, ID: id} }
func FolderObject(id string) ObjectRef    { return ObjectRef{Type: TypeFolder, ID: id} }

func (o ObjectRef) String() string  { return o.Type + ":" + o.ID }
func (s SubjectRef) String() string { return s.Type + ":" + s.ID }

// Grant is one explicit (subject, object, capability) row. It is the shape both
// audit directions return: GrantsFor fixes the subject, SubjectsOf fixes the
// object.
type Grant struct {
	Object      ObjectRef
	Subject     SubjectRef
	Capability  Capability
	WorkspaceID string
	GrantedBy   string // "" when the granting user has since been deleted
	CreatedAt   int64  // unix seconds
}

// ---------------------------------------------------------------------------
// Validation
// ---------------------------------------------------------------------------

// MaxPathDepth caps how deeply objects may nest. A path is text, and a
// pathological nesting depth makes every subtree predicate longer and every
// move more expensive; 32 is generous for a Drive and finite for the database
// (acl_object's CHECK enforces the same number).
const MaxPathDepth = 32

// typePattern is the same alphabet acl_object_type_shape enforces. The absence
// of '_' is not cosmetic: a path is spliced into LIKE predicates, and '_' is a
// single-character wildcard there, so a type named 'my_type' would make a
// subtree predicate match its siblings.
var typePattern = regexp.MustCompile(`^[a-z][a-z0-9]{0,31}$`)

// errNotUUID is returned by no exported function. Callers that hand this
// package an id from a URL get a deny, not an error: an unparseable uuid names
// no object, which is a 404, and turning it into a 500 would make every fuzzer
// look like an outage. A malformed TYPE is different — that value is a compiled
// constant, so a bad one is a bug and says so.
func validObjectType(t string) error {
	if !typePattern.MatchString(t) {
		return fmt.Errorf("invalid object type %q: must match %s", t, typePattern)
	}
	return nil
}

func validSubjectType(t string) error {
	if _, ok := subjectKeyPrefix[t]; !ok {
		return fmt.Errorf("invalid subject type %q", t)
	}
	return nil
}

// canonicalUUID re-renders an id in canonical lowercase hyphenated form. Every
// id that reaches a path, a key or a query goes through it, because both the
// path CHECK and the key CHECK are written against canonical form and a
// non-canonical id would be stored once and then never match.
func canonicalUUID(s string) (string, bool) {
	u, err := uuid.Parse(s)
	if err != nil {
		return "", false
	}
	return u.String(), true
}

// normalize canonicalises an object reference, reporting whether it can name
// anything at all. A bad type is an error (a bug); a bad id is simply "no such
// object".
func (o ObjectRef) normalize() (ObjectRef, bool, error) {
	if err := validObjectType(o.Type); err != nil {
		return ObjectRef{}, false, err
	}
	id, ok := canonicalUUID(o.ID)
	if !ok {
		return ObjectRef{}, false, nil
	}
	return ObjectRef{Type: o.Type, ID: id}, true, nil
}

func (s SubjectRef) normalize() (SubjectRef, bool, error) {
	if err := validSubjectType(s.Type); err != nil {
		return SubjectRef{}, false, err
	}
	id, ok := canonicalUUID(s.ID)
	if !ok {
		return SubjectRef{}, false, nil
	}
	return SubjectRef{Type: s.Type, ID: id}, true, nil
}

// Key renders the access key a subject holds by identity: u-<uuid> for a user,
// g-<uuid> for a group. It returns "" for anything that would not survive
// internal/search's validKey, and callers must drop empty keys — an unvalidated
// id must never reach a filter expression or a stored ACL.
func (s SubjectRef) Key() string {
	prefix, ok := subjectKeyPrefix[s.Type]
	if !ok {
		return ""
	}
	id, ok := canonicalUUID(s.ID)
	if !ok {
		return ""
	}
	return prefix + id
}

// ContainerKey renders the access key that grants read on everything inside a
// container object, or "" when the type is not a container.
//
// This is the bounded half of the access-key model: an object inside a
// container carries ONE key regardless of how many subjects may read the
// container, so a Drive with 100k files stores 100k keys and matches on the
// handful the caller holds.
func ContainerKey(o ObjectRef) string {
	prefix, ok := containerKeyPrefix[o.Type]
	if !ok {
		return ""
	}
	id, ok := canonicalUUID(o.ID)
	if !ok {
		return ""
	}
	return prefix + id
}

// WorkspaceKey renders the key every member of a workspace holds. It is the
// same string internal/search's WorkspaceKey produces; the two are separate
// functions only because internal/authz must not import internal/search.
func WorkspaceKey(workspaceID string) string {
	id, ok := canonicalUUID(workspaceID)
	if !ok {
		return ""
	}
	return "w-" + id
}

// ---------------------------------------------------------------------------
// Materialized paths
// ---------------------------------------------------------------------------

// pathSegment renders one '<type>:<uuid>' path component.
func pathSegment(o ObjectRef) string { return o.Type + ":" + o.ID }

// buildPath renders the path of an object under a parent path. The result
// always ends in '/', which is what makes a subtree exactly
// `path LIKE <path> || '%'` with no risk of '/workspace:a/channel:1' matching
// '/workspace:a/channel:10'.
func buildPath(parentPath string, o ObjectRef) string {
	return parentPath + pathSegment(o) + "/"
}

// pathSegments splits a stored path into its '<type>:<uuid>' components.
func pathSegments(path string) []string {
	trimmed := strings.Trim(path, "/")
	if trimmed == "" {
		return nil
	}
	return strings.Split(trimmed, "/")
}

// pathDepth counts the segments in a path.
func pathDepth(path string) int { return len(pathSegments(path)) }

// hasPrefixPath reports whether child is inside (or is) parent, on segment
// boundaries. Both arguments are stored paths, so both end in '/'.
func hasPrefixPath(child, parent string) bool {
	return strings.HasPrefix(child, parent)
}
