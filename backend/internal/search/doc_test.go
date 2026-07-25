package search

import (
	"strings"
	"testing"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
)

func TestAccessKeyConstruction(t *testing.T) {
	tests := []struct {
		name string
		got  string
		want string
	}{
		{"workspace", WorkspaceKey(wsA), "w-" + wsA},
		{"channel", ChannelKey(chA), "c-" + chA},
		{"user", UserKey(usr), "u-" + usr},
		{"group", GroupKey(wsA), "g-" + wsA},
		{"uppercase is canonicalised", ChannelKey(strings.ToUpper(hexID)), "c-" + hexID},
		{"non-uuid yields no key", ChannelKey("general"), ""},
		{"empty yields no key", ChannelKey(""), ""},
		{"injection attempt yields no key", ChannelKey(`" OR "1"="1`), ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.want {
				t.Errorf("key = %q, want %q", tt.got, tt.want)
			}
		})
	}
}

func TestValidKey(t *testing.T) {
	valid := []string{"w-" + wsA, "c-" + chA, "u-" + usr, "g-" + wsA}
	for _, k := range valid {
		if !validKey(k) {
			t.Errorf("validKey(%q) = false, want true", k)
		}
	}
	invalid := []string{
		"",
		chA,                                  // no prefix
		"x-" + chA,                           // unknown prefix
		"c-" + strings.ToUpper(hexID),        // not canonical (uppercase)
		"c-22222222222222222222222222222222", // not canonical (unhyphenated)
		"c-" + chA + `" OR "1"="1`,
		"c-",
		"w-*",
	}
	for _, k := range invalid {
		if validKey(k) {
			t.Errorf("validKey(%q) = true, want false", k)
		}
	}
}

// TestAuthzKeysPassValidation is the contract between internal/authz and this
// package, asserted from the accepting side.
//
// acl_key stores exactly what validKey accepts. It has to: the Meilisearch
// filter is built by string concatenation, and a key that fails validation is
// DROPPED — a dropped narrowing term widens the query, and a widened tenancy
// filter is a cross-tenant leak. An earlier draft of docs/plans/00-permissions.md
// specified a different encoding and five plans assumed a reconciliation that
// did not exist; this test is what makes the next such drift a build failure
// instead of an incident.
func TestAuthzKeysPassValidation(t *testing.T) {
	keys := map[string]string{
		"workspace": authz.WorkspaceKey(wsA),
		"channel":   authz.ContainerKey(authz.ChannelObject(chA)),
		"folder":    authz.ContainerKey(authz.ObjectRef{Type: authz.TypeFolder, ID: chA}),
		"user":      authz.UserSubject(usr).Key(),
		"group":     authz.GroupSubject(usr).Key(),
	}
	for name, key := range keys {
		if key == "" {
			t.Errorf("authz produced no %s key at all", name)
			continue
		}
		if !validKey(key) {
			t.Errorf("validKey(%q) = false for the authz %s key; a key search rejects is a key that vanishes from the filter", key, name)
		}
	}

	// And the constructors on this side produce the same strings, so neither
	// package can quietly redefine the encoding for the other.
	if got, want := FolderKey(chA), authz.ContainerKey(authz.ObjectRef{Type: authz.TypeFolder, ID: chA}); got != want {
		t.Errorf("FolderKey = %q, authz folder key = %q", got, want)
	}
	if got, want := ChannelKey(chA), authz.ContainerKey(authz.ChannelObject(chA)); got != want {
		t.Errorf("ChannelKey = %q, authz channel key = %q", got, want)
	}
	if got, want := UserKey(usr), authz.UserSubject(usr).Key(); got != want {
		t.Errorf("UserKey = %q, authz user key = %q", got, want)
	}
}

func TestFolderKeyIsAccepted(t *testing.T) {
	// f- is added ahead of Drive on purpose: nothing emits a folder key yet, and
	// a pillar that needed one later would have to widen a closed, security-
	// critical validator under deadline.
	if !validKey("f-" + chA) {
		t.Error("validKey rejected a folder key")
	}
}

func TestDocID(t *testing.T) {
	if got, want := DocID(TypeMessage, chA), "message_"+chA; got != want {
		t.Errorf("DocID = %q, want %q", got, want)
	}
	if got, want := DocID(TypeFile, strings.ToUpper(hexID)), "file_"+hexID; got != want {
		t.Errorf("DocID = %q, want %q", got, want)
	}
	// Meilisearch document ids may only contain [a-zA-Z0-9_-]; the separator has
	// to survive that rule, which is why it is not "message:<id>".
	if strings.ContainsAny(DocID(TypeMessage, chA), ":. /") {
		t.Error("doc id contains a character Meilisearch rejects")
	}
	if DocID("unknown", chA) != "" {
		t.Error("an unknown type must not produce a doc id")
	}
	if DocID(TypeMessage, "not-a-uuid") != "" {
		t.Error("a non-uuid object id must not produce a doc id")
	}
}

func TestObjectTypeParsing(t *testing.T) {
	if got, ok := ParseObjectType(" Message "); !ok || got != TypeMessage {
		t.Errorf("ParseObjectType = (%q, %v)", got, ok)
	}
	for _, bad := range []string{"", "messages", "*", "message,file"} {
		if _, ok := ParseObjectType(bad); ok {
			t.Errorf("ParseObjectType(%q) must be rejected", bad)
		}
	}
	for _, typ := range ObjectTypes {
		if !typ.Valid() {
			t.Errorf("%q is listed in ObjectTypes but is not Valid", typ)
		}
	}
}

func TestMessageDocIsChannelScoped(t *testing.T) {
	doc, err := MessageDoc{
		ID: chB, ChannelID: chA, WorkspaceID: wsA, UserID: usr,
		Content: "hello", CreatedAt: 1700000000,
	}.Doc().validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if doc.Type != TypeMessage || doc.DocID != "message_"+chB || doc.ID != chB {
		t.Fatalf("doc identity = %+v", doc)
	}
	// One key, and it is the channel: authz.Checker.KeysFor answers "public
	// channel in my workspace, or a channel I am in" with channel ids, so this
	// reproduces the old channel_id allowlist exactly. A workspace key here
	// would publish every private channel and DM to the whole workspace.
	if len(doc.ACL) != 1 || doc.ACL[0] != "c-"+chA {
		t.Fatalf("acl = %v, want exactly the channel key", doc.ACL)
	}
	if doc.Title != "" {
		t.Errorf("messages have no title, got %q", doc.Title)
	}
}

func TestFileDocACLFollowsAttachment(t *testing.T) {
	attached := FileDoc{
		ID: fil, WorkspaceID: wsA, ChannelID: chA, UserID: usr,
		Name: "budget.xlsx", CreatedAt: 1700000000,
	}.Doc()
	if len(attached.ACL) != 1 || attached.ACL[0] != "c-"+chA {
		t.Fatalf("attached file acl = %v, want the channel key", attached.ACL)
	}
	if attached.Title != "budget.xlsx" {
		t.Fatalf("title = %q, want the filename", attached.Title)
	}

	// An unattached upload is readable by its uploader alone — the same rule
	// file.Handler.canRead applies. A workspace key here would make every
	// in-flight upload searchable by every colleague.
	unattached := FileDoc{
		ID: fil, WorkspaceID: wsA, UserID: usr, Name: "draft.pdf", CreatedAt: 1700000000,
	}.Doc()
	if len(unattached.ACL) != 1 || unattached.ACL[0] != "u-"+usr {
		t.Fatalf("unattached file acl = %v, want the uploader key", unattached.ACL)
	}
}

// Every rejection here is a document that would have been written, acked and
// then been invisible — or, for the ACL cases, visible to the wrong people.
func TestDocValidateRejects(t *testing.T) {
	valid := Doc{
		Type: TypeMessage, ID: chB, WorkspaceID: wsA, ChannelID: chA, UserID: usr,
		ACL: []string{"c-" + chA}, CreatedAt: 1700000000,
	}
	if _, err := valid.validate(); err != nil {
		t.Fatalf("the baseline document must validate: %v", err)
	}

	tests := []struct {
		name   string
		mutate func(d *Doc)
	}{
		{"unknown type", func(d *Doc) { d.Type = "note" }},
		{"no object id", func(d *Doc) { d.ID = "" }},
		{"non-uuid object id", func(d *Doc) { d.ID = "42" }},
		{"no workspace", func(d *Doc) { d.WorkspaceID = "" }},
		{"non-uuid workspace", func(d *Doc) { d.WorkspaceID = "everyone" }},
		{"non-uuid channel", func(d *Doc) { d.ChannelID = "general" }},
		{"non-uuid user", func(d *Doc) { d.UserID = "me" }},
		{"empty acl", func(d *Doc) { d.ACL = nil }},
		{"malformed acl entry", func(d *Doc) { d.ACL = []string{"c-" + chA, "everyone"} }},
		{"no created_at", func(d *Doc) { d.CreatedAt = 0 }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			doc := valid
			doc.ACL = append([]string(nil), valid.ACL...)
			tt.mutate(&doc)
			_, err := doc.validate()
			if err == nil {
				t.Fatal("expected a rejection")
			}
			if !isPermanent(err) {
				t.Fatalf("error %v is retryable; redelivery cannot fix a bad document", err)
			}
		})
	}
}

func TestDocValidateCanonicalises(t *testing.T) {
	doc, err := Doc{
		Type:        TypeFile,
		ID:          strings.ToUpper(hexID),
		WorkspaceID: strings.ToUpper(wsA),
		ChannelID:   strings.ToUpper(hexID),
		UserID:      strings.ToUpper(usr),
		ACL:         []string{ChannelKey(strings.ToUpper(hexID))},
		CreatedAt:   1700000000,
	}.validate()
	if err != nil {
		t.Fatalf("validate: %v", err)
	}
	if doc.ID != hexID || doc.WorkspaceID != wsA || doc.ChannelID != hexID || doc.UserID != usr {
		t.Fatalf("ids were not canonicalised: %+v", doc)
	}
	if doc.DocID != "file_"+hexID {
		t.Fatalf("doc_id = %q", doc.DocID)
	}
}
