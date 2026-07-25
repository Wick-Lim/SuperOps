package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/search"
)

const (
	wsID     = "11111111-1111-1111-1111-111111111111"
	chID     = "22222222-2222-2222-2222-222222222222"
	objID    = "33333333-3333-3333-3333-333333333333"
	userID   = "44444444-4444-4444-4444-444444444444"
	folderID = "55555555-5555-5555-5555-555555555555"
	otherID  = "66666666-6666-6666-6666-666666666666"
)

func TestSelectSources(t *testing.T) {
	tests := []struct {
		name  string
		flag  string
		want  []search.ObjectType
		valid bool
	}{
		{"default", "", []search.ObjectType{search.TypeMessage, search.TypeFile}, true},
		{"all", "all", []search.ObjectType{search.TypeMessage, search.TypeFile}, true},
		{"one type", "file", []search.ObjectType{search.TypeFile}, true},
		{"the other type", "message", []search.ObjectType{search.TypeMessage}, true},
		// A type search knows about but nothing produces yet must not silently
		// rebuild to zero rows and report success.
		{"a type with no source", "document", nil, false},
		{"nonsense", "everything", nil, false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := selectSources(tt.flag)
			if (err == nil) != tt.valid {
				t.Fatalf("selectSources(%q) error = %v, want valid=%v", tt.flag, err, tt.valid)
			}
			if !tt.valid {
				return
			}
			if len(got) != len(tt.want) {
				t.Fatalf("selected %d sources, want %d", len(got), len(tt.want))
			}
			for i := range got {
				if got[i].typ != tt.want[i] {
					t.Errorf("source %d = %q, want %q", i, got[i].typ, tt.want[i])
				}
			}
		})
	}
}

// fakeRow feeds a source's scan function the values its SQL selects, in order.
type fakeRow []any

func (r fakeRow) Scan(dest ...any) error {
	if len(dest) != len(r) {
		return fmt.Errorf("scan into %d destinations, row has %d values", len(dest), len(r))
	}
	for i, v := range r {
		switch d := dest[i].(type) {
		case *string:
			s, ok := v.(string)
			if !ok {
				return fmt.Errorf("value %d is %T, want string", i, v)
			}
			*d = s
		case *[]string:
			ss, ok := v.([]string)
			if !ok {
				return fmt.Errorf("value %d is %T, want []string", i, v)
			}
			*d = ss
		case *time.Time:
			ts, ok := v.(time.Time)
			if !ok {
				return fmt.Errorf("value %d is %T, want time.Time", i, v)
			}
			*d = ts
		default:
			return fmt.Errorf("unsupported destination %T", dest[i])
		}
	}
	return nil
}

func sourceFor(t *testing.T, typ search.ObjectType) source {
	t.Helper()
	for _, s := range sources() {
		if s.typ == typ {
			return s
		}
	}
	t.Fatalf("no source for %q", typ)
	return source{}
}

// The row-to-document mapping is where a rebuild decides who can see what. It
// is checked here rather than only end-to-end, because a wrong column order
// produces a document that is valid and wrong.
func TestScanRows(t *testing.T) {
	createdAt := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name    string
		typ     search.ObjectType
		row     fakeRow
		wantACL []string
		want    search.Doc
	}{
		{
			name: "message is keyed on its channel",
			typ:  search.TypeMessage,
			// id, channel_id, workspace_id, user_id, content, created_at
			row:     fakeRow{objID, chID, wsID, userID, "hello world", createdAt},
			wantACL: []string{"c-" + chID},
			want: search.Doc{
				Type: search.TypeMessage, ID: objID, WorkspaceID: wsID, ChannelID: chID,
				UserID: userID, Content: "hello world", CreatedAt: createdAt.Unix(),
			},
		},
		{
			// THE CASE THE TOOL GOT WRONG. A Drive file's readers are the
			// materialized acl_key rows and nothing else: the workspace grant on
			// the Drive root, the folder it sits in, whoever it was shared with.
			// The query carries them verbatim and Doc() must pass them through
			// UNCHANGED — the moment it derives anything, a rebuild rewrites who
			// can see the file.
			name: "drive file carries its materialized keys verbatim",
			typ:  search.TypeFile,
			// id, workspace_id, user_id, name, channel_id, folder_id, file_type,
			// acl, created_at. The type is "spreadsheet" and the document must
			// come back as one: a rebuild that wrote it as a file would leave
			// ?type=spreadsheet empty for the whole corpus.
			row: fakeRow{objID, wsID, userID, "budget.xlsx", "", folderID, "spreadsheet",
				[]string{"w-" + wsID, "f-" + folderID, "u-" + otherID}, createdAt},
			wantACL: []string{"w-" + wsID, "f-" + folderID, "u-" + otherID},
			want: search.Doc{
				Type: search.TypeSpreadsheet, ID: objID, WorkspaceID: wsID, FolderID: folderID,
				UserID: userID, Title: "budget.xlsx", CreatedAt: createdAt.Unix(),
			},
		},
		{
			name: "attached file is keyed on the channel of its message",
			typ:  search.TypeFile,
			row: fakeRow{objID, wsID, userID, "plan.pdf", chID, "", "file",
				[]string{"c-" + chID}, createdAt},
			wantACL: []string{"c-" + chID},
			want: search.Doc{
				Type: search.TypeFile, ID: objID, WorkspaceID: wsID, ChannelID: chID,
				UserID: userID, Title: "plan.pdf", CreatedAt: createdAt.Unix(),
			},
		},
		{
			// A chat attachment predating the Drive migration has no acl_key row at
			// all, and the derivation is the ONLY thing that keeps it findable. It
			// stays, for exactly this case.
			name:    "unattached file with no materialized keys falls back to its uploader",
			typ:     search.TypeFile,
			row:     fakeRow{objID, wsID, userID, "draft.pdf", "", "", "file", []string(nil), createdAt},
			wantACL: []string{"u-" + userID},
			want: search.Doc{
				Type: search.TypeFile, ID: objID, WorkspaceID: wsID,
				UserID: userID, Title: "draft.pdf", CreatedAt: createdAt.Unix(),
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			src := sourceFor(t, tt.typ)
			doc, cur, err := src.scan(tt.row)
			if err != nil {
				t.Fatalf("scan: %v", err)
			}
			want := tt.want
			want.ACL = tt.wantACL
			if doc.Type != want.Type || doc.ID != want.ID || doc.WorkspaceID != want.WorkspaceID ||
				doc.ChannelID != want.ChannelID || doc.UserID != want.UserID ||
				doc.FolderID != want.FolderID ||
				doc.Title != want.Title || doc.Content != want.Content || doc.CreatedAt != want.CreatedAt {
				t.Fatalf("doc  = %+v\nwant = %+v", doc, want)
			}
			if len(doc.ACL) != len(want.ACL) {
				t.Fatalf("acl = %v, want %v", doc.ACL, want.ACL)
			}
			for i := range doc.ACL {
				if doc.ACL[i] != want.ACL[i] {
					t.Fatalf("acl = %v, want %v", doc.ACL, want.ACL)
				}
			}
			// The cursor is what makes the walk terminate; a scan that forgot to
			// advance it would loop over the first page forever.
			if !cur.at.Equal(createdAt) || cur.id != objID {
				t.Fatalf("cursor = %+v, want (%v, %s)", cur, createdAt, objID)
			}
		})
	}
}

// Every source must page with the same four parameters in the same order, or
// rebuild passes them to the wrong placeholders.
func TestSourceSQLSharesTheSameParameters(t *testing.T) {
	for _, src := range sources() {
		for _, want := range []string{"$1::uuid", "$2::timestamptz", "$3::uuid", "LIMIT $4", "ORDER BY"} {
			if !strings.Contains(src.sql, want) {
				t.Errorf("%s source sql does not use %s:\n%s", src.typ, want, src.sql)
			}
		}
	}
}
