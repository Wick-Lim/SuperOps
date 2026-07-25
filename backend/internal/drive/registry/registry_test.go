package registry

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"

	"github.com/Wick-Lim/SuperOps/backend/internal/search"
)

func collabKind(t string) Kind {
	return Kind{
		Type:        t,
		DisplayName: strings.ToUpper(t[:1]) + t[1:],
		Storage:     StorageCollab,
		New:         func(context.Context, pgx.Tx, NewRequest) error { return nil },
		// Every collab kind in the product sets this, so the golden literal
		// below carries it as true rather than testing a shape no registered
		// kind has.
		ClientProjected: true,
	}
}

// ONE VOCABULARY, THREE TABLES. file_type, collab_documents.resource_type and
// search.ObjectType are the same string, and nothing but this test stops them
// drifting — the two CHECK constraints are shape rules, not enumerations, so a
// type the search index does not know about inserts happily and then simply
// never appears in results.
func TestEveryKindIsAKnownSearchObjectType(t *testing.T) {
	r := New()
	// Every type a later phase will register, declared here so the drift is
	// caught when search.ObjectTypes changes rather than when that phase starts.
	for _, k := range []Kind{
		collabKind("document"),
		collabKind("spreadsheet"),
		collabKind("design"),
	} {
		if err := r.Register(k); err != nil {
			t.Fatalf("register %s: %v", k.Type, err)
		}
	}

	for _, k := range r.All() {
		if !search.ObjectType(k.Type).Valid() {
			t.Errorf("kind %q is not a search.ObjectType, so objects of that type would be "+
				"indexed under a type the search API rejects and would never be findable. "+
				"Add it to search.ObjectTypes in the same change.", k.Type)
		}
	}
}

func TestRegisterRefusesWhatTheDatabaseWouldRefuse(t *testing.T) {
	tests := []struct {
		name    string
		kind    Kind
		wantErr string
		because string
	}{
		{
			name:    "a type with a capital letter",
			kind:    Kind{Type: "Document", DisplayName: "D", Storage: StorageBlob},
			wantErr: "CHECK constraint",
			because: "files.file_type would reject it on the first insert, in a request",
		},
		{
			name:    "a type with a hyphen",
			kind:    Kind{Type: "my-type", DisplayName: "D", Storage: StorageBlob},
			wantErr: "CHECK constraint",
			because: "same shape rule",
		},
		{
			name:    "no display name",
			kind:    Kind{Type: "thing", Storage: StorageBlob},
			wantErr: "DisplayName",
			because: "the client renders it in a menu",
		},
		{
			name:    "an unknown storage mode",
			kind:    Kind{Type: "thing", DisplayName: "T", Storage: StorageMode("magic")},
			wantErr: "storage mode",
			because: "the zero value would otherwise register as a blob type by accident",
		},
		{
			name: "a versioned collab kind",
			kind: Kind{Type: "doc2", DisplayName: "D", Storage: StorageCollab, Versioned: true,
				New: func(context.Context, pgx.Tx, NewRequest) error { return nil }},
			wantErr: "silently discarded",
			because: "bytes posted to a CRDT-backed object are dropped by the next merge; refusing beats accepting",
		},
		{
			name:    "a collab kind with no New",
			kind:    Kind{Type: "doc3", DisplayName: "D", Storage: StorageCollab},
			wantErr: "no New",
			because: "nothing would create its collab_documents row and the editor would open on nothing",
		},
		{
			name: "an extension without a dot",
			kind: Kind{Type: "thing", DisplayName: "T", Storage: StorageBlob,
				Extensions: []string{"png"}},
			wantErr: "must start with a dot",
			because: "path.Ext returns \".png\", so the match would never fire",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := New().Register(tt.kind)
			if err == nil {
				t.Fatalf("Register succeeded, want an error — %s", tt.because)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Register = %v, want it to mention %q", err, tt.wantErr)
			}
		})
	}
}

func TestRegisterRefusesADuplicate(t *testing.T) {
	r := New()
	k := Kind{Type: "sheet", DisplayName: "Sheet", Storage: StorageBlob}
	if err := r.Register(k); err != nil {
		t.Fatal(err)
	}
	if err := r.Register(k); err == nil {
		t.Error("two editors claiming one type registered silently; whichever opened would be a race")
	}
}

// ForUpload never fails, and never returns a collab kind.
func TestForUploadFallsBackAndNeverPicksCollab(t *testing.T) {
	r := New()
	if err := r.Register(Kind{
		Type: "image", DisplayName: "Image", Storage: StorageBlob,
		Extensions: []string{".png", ".jpg"},
		MimeTypes:  []string{"image/png", "image/jpeg"},
	}); err != nil {
		t.Fatal(err)
	}
	// A collab kind that claims an extension anyway — the case the guard exists
	// for. Uploading bytes cannot produce a CRDT document, so a match here would
	// create an empty document and silently discard the file.
	doc := collabKind("document")
	doc.Extensions = []string{".md"}
	doc.MimeTypes = []string{"text/markdown"}
	if err := r.Register(doc); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name, file, contentType, want string
	}{
		{"by extension", "cat.png", "application/octet-stream", "image"},
		{"extension is case-insensitive", "CAT.PNG", "", "image"},
		{"by content type when the extension is unknown", "blob", "image/jpeg", "image"},
		{"content type with parameters", "blob", "image/png; charset=binary", "image"},
		{"unknown falls back to file", "notes.xyz", "application/x-thing", TypeFile},
		{"no name and no type still answers", "", "", TypeFile},
		{"a collab kind is never chosen by extension", "notes.md", "", TypeFile},
		{"a collab kind is never chosen by content type", "notes", "text/markdown", TypeFile},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := r.ForUpload(tt.file, tt.contentType); got.Type != tt.want {
				t.Errorf("ForUpload(%q, %q) = %q, want %q", tt.file, tt.contentType, got.Type, tt.want)
			}
		})
	}
}

// The descriptor list is a published contract: three later phases and the whole
// client dispatch read it. A change to its shape has to be deliberate, so this
// is a golden test — if it fails, update the literal ON PURPOSE and say why in
// the commit.
func TestRegistryDescriptorsAreAStableContract(t *testing.T) {
	r := New()
	if err := r.Register(StubDocumentForTest()); err != nil {
		t.Fatal(err)
	}

	got, err := json.MarshalIndent(r.Descriptors(), "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	const want = `[
  {
    "type": "file",
    "display_name": "File",
    "storage_mode": "blob",
    "extensions": [],
    "mime_types": [],
    "creatable": false,
    "versioned": true,
    "previewable": false,
    "client_projected": false
  },
  {
    "type": "document",
    "display_name": "Document",
    "storage_mode": "collab",
    "extensions": [],
    "mime_types": [],
    "creatable": true,
    "versioned": false,
    "previewable": false,
    "client_projected": true
  }
]`
	if string(got) != want {
		t.Errorf("GET /drive/registry payload changed.\n\ngot:\n%s\n\nwant:\n%s\n\n"+
			"Three later phases and every client dispatch read this. If the change is "+
			"intended, update the literal and say so in the commit message.", got, want)
	}
}

// StubDocumentForTest mirrors drive.DocumentKind without importing it
// (drive imports this package, so the dependency runs one way only).
func StubDocumentForTest() Kind {
	return Kind{
		Type:        "document",
		DisplayName: "Document",
		Storage:     StorageCollab,
		New:         func(context.Context, pgx.Tx, NewRequest) error { return nil },
		// Every collab kind in the product sets this, so the golden literal
		// below carries it as true rather than testing a shape no registered
		// kind has.
		ClientProjected: true,
	}
}

// A plain file is uploaded, never created: "New File" would produce an empty
// object with no way to put bytes into it except an upload that would have
// created its own row.
func TestPlainFileIsNotCreatable(t *testing.T) {
	for _, d := range New().Descriptors() {
		if d.Type == TypeFile && d.Creatable {
			t.Error("the fallback file kind is creatable; the client would offer New File, " +
				"which produces an empty object nothing can fill")
		}
	}
}

// The two refusals that make ONE projection mechanism a registration-time
// error rather than a review comment.
func TestProjectionRefusals(t *testing.T) {
	t.Run("a collab kind may not implement Text", func(t *testing.T) {
		k := StubDocumentForTest()
		k.Text = func(context.Context, io.Reader) (string, error) { return "", nil }
		err := New().Register(k)
		if err == nil {
			t.Fatal("registered a collab kind with a Text extractor; there is no byte stream " +
				"that IS a CRDT document, so this could only be the Go-side Yjs reader " +
				"migration 015 refuses")
		}
		if !strings.Contains(err.Error(), "ClientProjected") {
			t.Errorf("the error does not point at the alternative: %v", err)
		}
	})

	t.Run("a blob kind may not be client-projected", func(t *testing.T) {
		err := New().Register(Kind{
			Type:            "report",
			DisplayName:     "Report",
			Storage:         StorageBlob,
			ClientProjected: true,
		})
		if err == nil {
			t.Fatal("registered a blob kind as client-projected; its bytes are on disk, so " +
				"trusting a client's description of them would let the last caller with " +
				"write capability decide what the search index says about it")
		}
	})

	t.Run("collab plus ClientProjected is the shipped shape", func(t *testing.T) {
		if err := New().Register(StubDocumentForTest()); err != nil {
			t.Fatalf("the shape every editor uses was refused: %v", err)
		}
	})
}
