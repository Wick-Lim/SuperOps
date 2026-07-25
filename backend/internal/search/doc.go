package search

import (
	"fmt"
	"strings"
)

// ---------------------------------------------------------------------------
// The object model
// ---------------------------------------------------------------------------

// ObjectType names the kind of thing a document describes. One index holds all
// of them, so `type` is both a filter and the discriminator a client uses to
// decide how to render a hit.
//
// The set is closed on purpose: an unknown type reaching the index would be
// invisible to every query that narrows by type, and an unknown type reaching a
// query would be silently dropped from the filter — which widens the result set
// rather than narrowing it. Both are rejected instead.
type ObjectType string

const (
	TypeMessage     ObjectType = "message"
	TypeFile        ObjectType = "file"
	TypeDocument    ObjectType = "document"
	TypeSpreadsheet ObjectType = "spreadsheet"
	TypeDesign      ObjectType = "design"
	TypeIssue       ObjectType = "issue"
)

// ObjectTypes is every type the index understands, in the order a caller who
// asks for "everything" gets them documented. Only message and file have a
// producer today; the rest are declared so a pillar landing later adds a doc
// constructor and a reindex source, not a schema decision.
var ObjectTypes = []ObjectType{
	TypeMessage, TypeFile, TypeDocument, TypeSpreadsheet, TypeDesign, TypeIssue,
}

// Valid reports whether t is one of the known object types.
func (t ObjectType) Valid() bool {
	for _, known := range ObjectTypes {
		if t == known {
			return true
		}
	}
	return false
}

// ParseObjectType turns caller input into a type, rejecting anything unknown.
func ParseObjectType(s string) (ObjectType, bool) {
	t := ObjectType(strings.ToLower(strings.TrimSpace(s)))
	return t, t.Valid()
}

// ---------------------------------------------------------------------------
// Access keys — the ACL representation, and why it is this one
// ---------------------------------------------------------------------------
//
// A document carries the set of *keys* that grant read access to it. A query
// carries the set of keys the caller holds. A hit requires the two sets to
// intersect:
//
//	acl IN ["w-<workspace>", "c-<channel>", ..., "u-<caller>"]
//
// Both sides are small and bounded by *memberships*, never by the number of
// objects: a Drive with 100k files shared workspace-wide stores one key per
// file and matches on the single workspace key the caller holds.
//
// The alternative that was rejected: keep passing the caller's readable object
// ids into the filter (what message search does today, with channel_id IN
// [...]). That works while the only container is a channel and a workspace has
// tens of them. It does not survive object-level ACLs — Phase 0 of the roadmap
// — because the readable set becomes "every file, doc and issue shared with
// me", which is unbounded, has to be computed in Postgres on every keystroke,
// and eventually exceeds Meilisearch's filter size at exactly the point the
// product is worth using. Access keys move that cost to write time, where it is
// paid once per object instead of once per query.
//
// Two other options considered and rejected:
//
//   - One index per type, queried with a federated multi-search. Ranking across
//     indexes is then Meilisearch's federation feature rather than one BM25
//     ordering, per-type settings drift, and every new pillar is a new index to
//     create, migrate and reconcile. A single index with a `type` filter gives
//     the same narrowing for free.
//   - Filtering after the fact in Postgres. Correct, and it throws away
//     Meilisearch's ranking: you cannot ask for "the best 20 hits I may read"
//     without over-fetching by an unknown factor, and the factor is worst
//     exactly for the users with the least access.
//
// Rules that make the key model safe:
//
//  1. An object with no keys is unreachable. That is fail-closed, and it is
//     also always a bug, so indexing one is a permanent error rather than a
//     silent write.
//  2. A caller with no keys gets no results. buildFilter refuses to produce a
//     filter, and Search synthesises an empty result rather than running an
//     unconstrained query. This is what TestCrossTenantSearch rests on.
//  3. Keys are opaque, validated strings. Every key is `<prefix>-<uuid>` and is
//     re-validated on the way into a filter expression, because the filter is
//     built by string concatenation.
//  4. Key derivation is never hand-written SQL. The caller's keys come from
//     internal/authz (AccessKeys, below); an object's keys come from the doc
//     constructor for its type.
const (
	// keyWorkspace grants to every member of a workspace — the "shared with the
	// whole workspace" case, and what public-channel content and workspace-wide
	// Drive objects will use.
	keyWorkspace = "w-"
	// keyChannel grants to everyone who may read a channel. Membership of a
	// private channel and workspace membership of a public one both resolve to
	// this key via authz.Checker.KeysFor, so a message's ACL does not have to
	// know whether its channel is public.
	keyChannel = "c-"
	// keyUser grants to exactly one user: an explicit share, or an object that
	// only its owner can see (an unattached upload).
	keyUser = "u-"
	// keyGroup is reserved for when authz grows teams/groups. Declared here so
	// the prefix namespace is decided in one place rather than by whoever adds
	// groups first.
	keyGroup = "g-"
	// keyFolder grants to everyone who may read a Drive folder, and is the
	// bounded encoding every editor will index against: a folder shared with 50
	// people puts ONE key on each document inside it, and the caller holds one
	// key per folder they can read rather than one per object.
	//
	// Nothing emits a folder key yet — folders do not exist until Drive
	// (ROADMAP phase 1). The prefix is added ahead of them on purpose: this set
	// is closed and validated on both write and query, so a pillar that needed
	// it later would have to widen the validator under deadline, and a key that
	// fails validation is DROPPED — which widens the filter it was supposed to
	// narrow. internal/authz already stores f- keys in acl_key for the same
	// reason.
	keyFolder = "f-"
)

// keyPrefixes is the closed set of prefixes validKey accepts. It must stay in
// sync with acl_key's CHECK constraint (migrations/016) — the two are the write
// and read ends of one contract.
var keyPrefixes = []string{keyWorkspace, keyChannel, keyUser, keyGroup, keyFolder}

// WorkspaceKey, ChannelKey, UserKey, GroupKey and FolderKey build access keys.
// Each returns "" when the id is not a uuid, and callers must drop empty keys —
// an unvalidated id must never reach a filter expression or a stored ACL.
func WorkspaceKey(id string) string { return objectKey(keyWorkspace, id) }
func ChannelKey(id string) string   { return objectKey(keyChannel, id) }
func UserKey(id string) string      { return objectKey(keyUser, id) }
func GroupKey(id string) string     { return objectKey(keyGroup, id) }
func FolderKey(id string) string    { return objectKey(keyFolder, id) }

func objectKey(prefix, id string) string {
	canonical, ok := canonicalUUID(id)
	if !ok {
		return ""
	}
	return prefix + canonical
}

// validKey re-checks a key on the boundary of the filter expression and of a
// stored document. Keys are produced by the constructors above, so this is the
// second of two layers rather than the only one.
func validKey(k string) bool {
	for _, prefix := range keyPrefixes {
		if rest, found := strings.CutPrefix(k, prefix); found {
			canonical, ok := canonicalUUID(rest)
			return ok && canonical == rest
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// The stored document
// ---------------------------------------------------------------------------

// Doc is one searchable object of any type.
//
// DocID is the Meilisearch primary key and is derived, never supplied: object
// ids are only unique within a type, and Meilisearch ids may only contain
// [a-zA-Z0-9_-], which rules out the obvious "type:id" spelling.
//
// ID stays the plain object id so a client can route a hit straight back to the
// object (and so message search results are byte-for-byte what they were).
type Doc struct {
	DocID       string     `json:"doc_id"`
	Type        ObjectType `json:"type"`
	ID          string     `json:"id"`
	WorkspaceID string     `json:"workspace_id"`
	// ChannelID is the channel the object hangs off, empty for object types that
	// are not channel-scoped. It exists as its own field — rather than being
	// inferred from the ACL — because "search only #general" is a narrowing the
	// caller asks for, not an access decision. Drive folders will get their own
	// field; overloading this one would make a folder id look like a channel id
	// to every filter that already exists.
	ChannelID string `json:"channel_id"`
	// FolderID is the Drive folder an object lives in, empty for everything that
	// is not in Drive. Its own field rather than an overload of ChannelID, for
	// the reason the comment above already gives: a folder id in the channel
	// field would look like a channel id to every filter that already exists.
	FolderID string `json:"folder_id"`
	// UserID is the author or owner: the message sender, the file uploader, the
	// document creator. It backs the `from` filter.
	UserID string `json:"user_id"`
	// Title is empty for types that have none (a message). Searchable, and ranked
	// above Content because it is the shorter, more selective field.
	Title   string `json:"title"`
	Content string `json:"content"`
	// ACL is the set of access keys that grant read on this object. Never
	// returned to a client: it can name users an object was shared with.
	ACL       []string `json:"acl"`
	CreatedAt int64    `json:"created_at"`
	IsDeleted bool     `json:"is_deleted"`
}

// DocID builds the primary key for an object. It returns "" when the type is
// unknown or the id is not a uuid, which validate turns into a permanent error.
func DocID(t ObjectType, objectID string) string {
	canonical, ok := canonicalUUID(objectID)
	if !ok || !t.Valid() {
		return ""
	}
	return string(t) + "_" + canonical
}

// validate canonicalises a document and rejects anything the index could not
// serve correctly.
//
// The failures here are all "would be written, acked, and then invisible":
// a non-canonical workspace id never matches the workspace filter, and an empty
// ACL can never intersect a caller's key set. Silently storing either is worse
// than refusing, so this returns a PermanentError — the durable consumer
// terminates the event instead of redelivering it five times.
func (d Doc) validate() (Doc, error) {
	if !d.Type.Valid() {
		return Doc{}, &PermanentError{Reason: fmt.Sprintf("unknown object type %q", d.Type)}
	}
	id, ok := canonicalUUID(d.ID)
	if !ok {
		return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s document has an invalid id %q", d.Type, d.ID)}
	}
	d.ID = id
	d.DocID = DocID(d.Type, id)

	workspaceID, ok := canonicalUUID(d.WorkspaceID)
	if !ok {
		return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s has an invalid workspace id %q", d.Type, id, d.WorkspaceID)}
	}
	d.WorkspaceID = workspaceID

	// Optional scalars: present or absent, never malformed.
	if d.ChannelID != "" {
		channelID, ok := canonicalUUID(d.ChannelID)
		if !ok {
			return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s has an invalid channel id %q", d.Type, id, d.ChannelID)}
		}
		d.ChannelID = channelID
	}
	if d.UserID != "" {
		userID, ok := canonicalUUID(d.UserID)
		if !ok {
			return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s has an invalid user id %q", d.Type, id, d.UserID)}
		}
		d.UserID = userID
	}

	acl := make([]string, 0, len(d.ACL))
	for _, key := range d.ACL {
		if !validKey(key) {
			return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s carries a malformed access key %q", d.Type, id, key)}
		}
		acl = append(acl, key)
	}
	if len(acl) == 0 {
		return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s has an empty access key set; nobody could ever find it", d.Type, id)}
	}
	d.ACL = acl

	if d.CreatedAt == 0 {
		return Doc{}, &PermanentError{Reason: fmt.Sprintf("%s %s has no created_at; it would sort before every real object", d.Type, id)}
	}
	return d, nil
}

// ---------------------------------------------------------------------------
// Per-type constructors
// ---------------------------------------------------------------------------

// MessageDoc is the message projection of Doc. It is what the indexer and the
// reindex message source build, and it is the shape the rest of the tree
// already speaks.
type MessageDoc struct {
	ID          string `json:"id"`
	ChannelID   string `json:"channel_id"`
	WorkspaceID string `json:"workspace_id"`
	UserID      string `json:"user_id"`
	Content     string `json:"content"`
	CreatedAt   int64  `json:"created_at"`
	// IsDeleted mirrors messages.is_deleted so soft-deleted content stops being
	// searchable. Documents written before this field existed simply lack it,
	// which the "NOT is_deleted = true" filter still treats as visible.
	IsDeleted bool `json:"is_deleted"`
}

// Doc renders a message as an indexable object.
//
// The ACL is exactly the channel key: authz.Checker.KeysFor already answers
// "public channel in a workspace I belong to, or a channel I am a member of"
// with a channel id list, so one key per message reproduces today's
// `channel_id IN [...]` filter exactly — including for DMs, which are channels.
func (m MessageDoc) Doc() Doc {
	return Doc{
		Type:        TypeMessage,
		ID:          m.ID,
		WorkspaceID: m.WorkspaceID,
		ChannelID:   m.ChannelID,
		UserID:      m.UserID,
		Content:     m.Content,
		ACL:         keySet(ChannelKey(m.ChannelID)),
		CreatedAt:   m.CreatedAt,
		IsDeleted:   m.IsDeleted,
	}
}

// FileDoc is the Drive/attachment projection of Doc.
//
// ChannelID is the channel of the message the file is attached to, and empty
// while it is unattached. That single field decides the ACL, and it mirrors
// file.Handler.canRead exactly: an attached file is readable by whoever may
// read its channel, an unattached one only by its uploader. Workspace
// membership is deliberately NOT enough — that gap is what let any member fetch
// a file posted in a private channel given its uuid.
type FileDoc struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	ChannelID   string `json:"channel_id"`
	// FolderID is set for a Drive file. It backs the "search inside this folder"
	// narrowing and is not an access decision.
	FolderID string `json:"folder_id"`
	UserID   string `json:"user_id"`
	Name     string `json:"name"`
	// ACL is the object's acl_key rows, verbatim.
	//
	// It exists because the derivation below cannot express Drive. A Drive file
	// is readable through its folder, through the workspace grant on the Drive
	// root, through a per-object grant, or through a channel it was shared into
	// — and "channel key, else user key" collapses all of that to one key that
	// is usually the wrong one. Reading the materialized rows means the index and
	// the database answer the same question, which is the only way a search
	// filter can be trusted as a tenancy boundary.
	//
	// Empty falls back to the derivation, so a message attachment indexed before
	// acl_key existed for it still gets the answer file.Handler.canRead gives.
	ACL       []string `json:"acl"`
	CreatedAt int64    `json:"created_at"`
	IsDeleted bool     `json:"is_deleted"`
}

// Doc renders a file as an indexable object. The filename is the title; the
// body stays empty until something extracts text from the object itself.
func (f FileDoc) Doc() Doc {
	acl := keySet(f.ACL...)
	if len(acl) == 0 {
		// The pre-Drive derivation, kept as the fallback rather than deleted:
		// it is exactly file.Handler.canRead, and a document whose materialized
		// keys have not arrived yet must narrow to the uploader rather than
		// widen to nothing. An empty ACL is a document validate rejects, which
		// is the fail-closed end of this.
		acl = keySet(ChannelKey(f.ChannelID))
		if len(acl) == 0 {
			acl = keySet(UserKey(f.UserID))
		}
	}
	return Doc{
		Type:        TypeFile,
		ID:          f.ID,
		WorkspaceID: f.WorkspaceID,
		ChannelID:   f.ChannelID,
		FolderID:    f.FolderID,
		UserID:      f.UserID,
		Title:       f.Name,
		ACL:         acl,
		CreatedAt:   f.CreatedAt,
		IsDeleted:   f.IsDeleted,
	}
}

// keySet drops the empty results of a failed key construction, so a malformed
// id becomes "no key" (and then, in validate, a rejected document) rather than
// an empty string in a filter expression.
func keySet(keys ...string) []string {
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		if k != "" {
			out = append(out, k)
		}
	}
	return out
}
