package message

import (
	"strings"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

func TestPaginate(t *testing.T) {
	at := time.Date(2026, 7, 25, 10, 0, 0, 0, time.UTC)

	tests := []struct {
		name     string
		base     string
		args     []any
		timeCol  string
		idCol    string
		asc      bool
		cur      httputil.Cursor
		limit    int
		wantSQL  string
		wantArgs []any
	}{
		{
			name:     "first page omits the keyset predicate",
			base:     "SELECT 1 FROM messages WHERE channel_id = $1",
			args:     []any{"chan"},
			timeCol:  "created_at",
			idCol:    "id",
			limit:    50,
			wantSQL:  "SELECT 1 FROM messages WHERE channel_id = $1 ORDER BY created_at DESC, id DESC LIMIT $2",
			wantArgs: []any{"chan", 50},
		},
		{
			name:     "descending page compares the (time, id) tuple",
			base:     "SELECT 1 FROM messages WHERE channel_id = $1",
			args:     []any{"chan"},
			timeCol:  "created_at",
			idCol:    "id",
			cur:      httputil.Cursor{CreatedAt: at, ID: "m1"},
			limit:    25,
			wantSQL:  "SELECT 1 FROM messages WHERE channel_id = $1 AND (created_at, id) < ($2, $3) ORDER BY created_at DESC, id DESC LIMIT $4",
			wantArgs: []any{"chan", at, "m1", 25},
		},
		{
			name:     "ascending page flips both the comparison and the ordering",
			base:     "SELECT 1 FROM messages WHERE parent_id = $1",
			args:     []any{"parent"},
			timeCol:  "created_at",
			idCol:    "id",
			asc:      true,
			cur:      httputil.Cursor{CreatedAt: at, ID: "m1"},
			limit:    10,
			wantSQL:  "SELECT 1 FROM messages WHERE parent_id = $1 AND (created_at, id) > ($2, $3) ORDER BY created_at ASC, id ASC LIMIT $4",
			wantArgs: []any{"parent", at, "m1", 10},
		},
		{
			name:     "placeholders continue after every existing argument",
			base:     "SELECT 1 FROM messages WHERE channel_id = $1 AND user_id = $2",
			args:     []any{"chan", "user"},
			timeCol:  "scheduled_at",
			idCol:    "id",
			asc:      true,
			cur:      httputil.Cursor{CreatedAt: at, ID: "m9"},
			limit:    5,
			wantSQL:  "SELECT 1 FROM messages WHERE channel_id = $1 AND user_id = $2 AND (scheduled_at, id) > ($3, $4) ORDER BY scheduled_at ASC, id ASC LIMIT $5",
			wantArgs: []any{"chan", "user", at, "m9", 5},
		},
		{
			name:     "qualified columns are preserved for joined queries",
			base:     "SELECT 1 FROM bookmarks b WHERE b.user_id = $1",
			args:     []any{"user"},
			timeCol:  "b.created_at",
			idCol:    "b.message_id",
			cur:      httputil.Cursor{CreatedAt: at, ID: "m3"},
			limit:    50,
			wantSQL:  "SELECT 1 FROM bookmarks b WHERE b.user_id = $1 AND (b.created_at, b.message_id) < ($2, $3) ORDER BY b.created_at DESC, b.message_id DESC LIMIT $4",
			wantArgs: []any{"user", at, "m3", 50},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotSQL, gotArgs := paginate(tc.base, tc.args, tc.timeCol, tc.idCol, tc.asc, tc.cur, tc.limit)
			if normalizeSQL(gotSQL) != tc.wantSQL {
				t.Errorf("sql:\n got %q\nwant %q", normalizeSQL(gotSQL), tc.wantSQL)
			}
			if len(gotArgs) != len(tc.wantArgs) {
				t.Fatalf("args: got %v, want %v", gotArgs, tc.wantArgs)
			}
			for i := range gotArgs {
				if gotArgs[i] != tc.wantArgs[i] {
					t.Errorf("arg %d: got %v, want %v", i, gotArgs[i], tc.wantArgs[i])
				}
			}
		})
	}
}

// TestPaginateArgsDoNotAliasCaller guards the append-to-caller-slice trap: the
// same base args slice is reused across list calls in tests and callers.
func TestPaginateArgsDoNotAliasCaller(t *testing.T) {
	args := make([]any, 1, 8)
	args[0] = "chan"
	cur := httputil.Cursor{CreatedAt: time.Now(), ID: "m1"}

	_, first := paginate("SELECT 1 WHERE a = $1", args, "created_at", "id", false, cur, 10)
	_, second := paginate("SELECT 1 WHERE a = $1", args, "created_at", "id", false, httputil.Cursor{}, 10)

	if len(first) != 4 {
		t.Fatalf("first call: got %d args, want 4", len(first))
	}
	if len(second) != 2 {
		t.Fatalf("second call: got %d args, want 2", len(second))
	}
	if first[1] != cur.CreatedAt || first[2] != "m1" {
		t.Errorf("first call args were overwritten by the second: %v", first)
	}
}

func TestLiveOnlyIsAppliedToEveryVisibleRead(t *testing.T) {
	// A soft-deleted or not-yet-sent row must never be served to a member.
	if !strings.Contains(liveOnly, "is_deleted = FALSE") || !strings.Contains(liveOnly, "is_scheduled = FALSE") {
		t.Fatalf("liveOnly no longer filters both states: %q", liveOnly)
	}
}

func TestPrefixed(t *testing.T) {
	got := normalizeSQL(prefixed("id, channel_id,\n\tuser_id", "m"))
	want := "m.id, m.channel_id, m.user_id"
	if got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestScanArgsMatchesMessageColumns(t *testing.T) {
	columns := strings.Split(messageColumns, ",")
	if len(columns) != len(scanArgs(&Message{})) {
		t.Fatalf("messageColumns has %d columns but scanArgs has %d destinations — "+
			"positional scanning would mis-bind", len(columns), len(scanArgs(&Message{})))
	}
}

func TestDedupe(t *testing.T) {
	tests := []struct {
		name string
		in   []string
		want []string
	}{
		{name: "nil stays nil", in: nil, want: nil},
		{name: "duplicates collapse", in: []string{"a", "b", "a"}, want: []string{"a", "b"}},
		{name: "empty ids are dropped", in: []string{"", "a", ""}, want: []string{"a"}},
		{name: "order is preserved", in: []string{"c", "a", "b"}, want: []string{"c", "a", "b"}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := dedupe(tc.in)
			if len(got) != len(tc.want) {
				t.Fatalf("got %v, want %v", got, tc.want)
			}
			for i := range got {
				if got[i] != tc.want[i] {
					t.Fatalf("got %v, want %v", got, tc.want)
				}
			}
		})
	}
}

func TestAttachReactions(t *testing.T) {
	withReactions := &Message{ID: "m1"}
	without := &Message{ID: "m2"}
	messages := []*Message{withReactions, without}

	attachReactions(messages, []*Reaction{
		{ID: "r1", MessageID: "m1", Emoji: "👍"},
		{ID: "r2", MessageID: "m1", Emoji: "🎉"},
		{ID: "r3", MessageID: "unknown", Emoji: "👀"},
	})

	if len(withReactions.Reactions) != 2 {
		t.Errorf("m1: got %d reactions, want 2", len(withReactions.Reactions))
	}
	// The bug this guards: a nil slice plus `omitempty` made the key vanish
	// from the payload instead of serializing as [].
	if without.Reactions == nil {
		t.Error("m2: reactions must be an empty slice, not nil")
	}
	if len(without.Reactions) != 0 {
		t.Errorf("m2: got %d reactions, want 0", len(without.Reactions))
	}
}

func TestAttachReactionsResetsPreviousHydration(t *testing.T) {
	m := &Message{ID: "m1", Reactions: []*Reaction{{ID: "stale", MessageID: "m1"}}}
	attachReactions([]*Message{m}, nil)
	if len(m.Reactions) != 0 {
		t.Errorf("stale reactions survived re-hydration: %v", m.Reactions)
	}
}

func TestAttachFiles(t *testing.T) {
	original := &Message{ID: "m1"}
	plain := &Message{ID: "m2"}
	forward := &Message{ID: "m3", Metadata: Metadata{ForwardedFrom: &ForwardRef{MessageID: "m1"}}}
	messages := []*Message{original, plain, forward}

	attachFiles(messages, []attachedFile{
		{MessageID: "m1", Ref: &FileRef{ID: "f1", Name: "spec.pdf"}},
	})

	if len(original.Files) != 1 {
		t.Fatalf("m1: got %d files, want 1", len(original.Files))
	}
	// A forward shows the source's attachments instead of dropping them.
	if len(forward.Files) != 1 || forward.Files[0].ID != "f1" {
		t.Errorf("forward: got %v, want the source's file", forward.Files)
	}
	if plain.Files == nil {
		t.Error("m2: files must be an empty slice, not nil")
	}
	if len(plain.Files) != 0 {
		t.Errorf("m2: got %d files, want 0", len(plain.Files))
	}
}

func TestFileSourceID(t *testing.T) {
	tests := []struct {
		name string
		msg  *Message
		want string
	}{
		{name: "plain message uses its own id", msg: &Message{ID: "m1"}, want: "m1"},
		{
			name: "forward uses the source id",
			msg:  &Message{ID: "m2", Metadata: Metadata{ForwardedFrom: &ForwardRef{MessageID: "m1"}}},
			want: "m1",
		},
		{
			name: "forward with an empty source falls back to its own id",
			msg:  &Message{ID: "m3", Metadata: Metadata{ForwardedFrom: &ForwardRef{}}},
			want: "m3",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := fileSourceID(tc.msg); got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// normalizeSQL collapses the whitespace of a generated statement so tests can
// assert on structure without pinning indentation.
func normalizeSQL(s string) string {
	return strings.Join(strings.Fields(s), " ")
}
