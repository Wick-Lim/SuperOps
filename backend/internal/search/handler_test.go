package search

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/pkg/authctx"
)

// world is one workspace with a public and a private channel, a message and a
// file in each, plus an unattached upload — and a second workspace whose owner
// must never see any of it.
type world struct {
	handler *Handler

	needle string

	ws, otherWS                       string
	insider, outsider, stranger       string
	pub, priv                         string
	pubMsg, privMsg                   string
	pubFile, privFile, unattachedFile string
}

func seedWorld(t *testing.T) world {
	t.Helper()
	pool := testDB(t)
	svc := testService(t)
	ctx := t.Context()

	w := world{
		needle:         "zoltar" + strings.ReplaceAll(uuid.NewString(), "-", ""),
		ws:             uuid.NewString(),
		otherWS:        uuid.NewString(),
		insider:        uuid.NewString(),
		outsider:       uuid.NewString(),
		stranger:       uuid.NewString(),
		pub:            uuid.NewString(),
		priv:           uuid.NewString(),
		pubMsg:         uuid.NewString(),
		privMsg:        uuid.NewString(),
		pubFile:        uuid.NewString(),
		privFile:       uuid.NewString(),
		unattachedFile: uuid.NewString(),
	}

	exec := func(sql string, args ...any) {
		t.Helper()
		if _, err := pool.Exec(ctx, sql, args...); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	for _, id := range []string{w.insider, w.outsider, w.stranger} {
		// Explicit casts: the same placeholder is a uuid in one column and text
		// in the next, which pgx will not infer.
		exec(`INSERT INTO users (id, email, username) VALUES ($1::uuid, $1::text || '@test.invalid', $1::text)`, id)
	}
	exec(`INSERT INTO workspaces (id, name, slug, owner_id)
	      VALUES ($1::uuid,'Acme',$1::text,$2), ($3::uuid,'Other',$3::text,$4)`,
		w.ws, w.insider, w.otherWS, w.stranger)
	exec(`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1,$2,'owner'), ($1,$3,'member'), ($4,$5,'owner')`,
		w.ws, w.insider, w.outsider, w.otherWS, w.stranger)
	exec(`INSERT INTO channels (id, workspace_id, name, slug, type, creator_id)
	      VALUES ($1::uuid,$2,'general',$1::text,'public',$3), ($4::uuid,$2,'secret',$4::text,'private',$3)`,
		w.pub, w.ws, w.insider, w.priv)
	// Only the insider is in the private channel; nobody has to join a public
	// one to read it, which is exactly what the c- key encodes.
	exec(`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1,$2,'admin')`, w.priv, w.insider)

	// The documents. They are written the way the worker's indexer writes them,
	// because nothing in an API process ever writes to the index.
	now := time.Now().Unix()
	index := func(doc Doc) {
		t.Helper()
		if err := svc.IndexAwait(ctx, doc); err != nil {
			t.Fatalf("index fixture: %v", err)
		}
		t.Cleanup(func() {
			cctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			if err := svc.DeleteAwait(cctx, doc.Type, doc.ID); err != nil {
				t.Errorf("cleanup %s %s: %v", doc.Type, doc.ID, err)
			}
		})
	}
	index(MessageDoc{ID: w.pubMsg, ChannelID: w.pub, WorkspaceID: w.ws, UserID: w.insider,
		Content: "public " + w.needle, CreatedAt: now}.Doc())
	index(MessageDoc{ID: w.privMsg, ChannelID: w.priv, WorkspaceID: w.ws, UserID: w.insider,
		Content: "private " + w.needle, CreatedAt: now}.Doc())
	index(FileDoc{ID: w.pubFile, WorkspaceID: w.ws, ChannelID: w.pub, UserID: w.insider,
		Name: w.needle + "-public.pdf", CreatedAt: now}.Doc())
	index(FileDoc{ID: w.privFile, WorkspaceID: w.ws, ChannelID: w.priv, UserID: w.insider,
		Name: w.needle + "-secret.pdf", CreatedAt: now}.Doc())
	index(FileDoc{ID: w.unattachedFile, WorkspaceID: w.ws, UserID: w.insider,
		Name: w.needle + "-draft.pdf", CreatedAt: now}.Doc())

	w.handler = NewHandler(svc, authz.New(pool))
	return w
}

// get drives the real handler, with the user id where the auth middleware would
// have put it.
func (w world) get(t *testing.T, userID, workspaceID string, params url.Values) (int, string) {
	t.Helper()
	target := "/api/v1/workspaces/" + workspaceID + "/search?" + params.Encode()
	req := httptest.NewRequest(http.MethodGet, target, nil)
	req.SetPathValue("workspace_id", workspaceID)
	req = req.WithContext(authctx.WithUserID(t.Context(), userID))

	rec := httptest.NewRecorder()
	w.handler.Search(rec, req)
	return rec.Code, rec.Body.String()
}

// hits runs a successful search and returns "<type>:<object id>" per hit.
func (w world) hits(t *testing.T, userID, workspaceID string, params url.Values) []string {
	t.Helper()
	params.Set("q", w.needle)
	code, body := w.get(t, userID, workspaceID, params)
	if code != http.StatusOK {
		t.Fatalf("status %d, body %s", code, body)
	}
	// httputil.JSON wraps every successful body in {"data": ...}.
	var res struct {
		Data struct {
			Hits []Hit `json:"hits"`
		} `json:"data"`
	}
	if err := json.Unmarshal([]byte(body), &res); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	out := make([]string, 0, len(res.Data.Hits))
	for _, h := range res.Data.Hits {
		out = append(out, string(h.Type)+":"+h.ID)
	}
	sort.Strings(out)
	return out
}

func sorted(items ...string) []string {
	sort.Strings(items)
	return items
}

// One query, every type, filtered by what the caller may actually read. This is
// the feature and the security boundary in one assertion.
func TestSearchSpansTypesAndRespectsAccess(t *testing.T) {
	w := seedWorld(t)

	tests := []struct {
		name   string
		user   string
		ws     string
		params url.Values
		want   []string
	}{
		{
			name: "a member of both channels sees every object",
			user: w.insider, ws: w.ws, params: url.Values{},
			want: sorted("message:"+w.pubMsg, "message:"+w.privMsg,
				"file:"+w.pubFile, "file:"+w.privFile, "file:"+w.unattachedFile),
		},
		{
			// Workspace membership is not permission to read every channel, and
			// it is not permission to read someone else's unattached upload.
			name: "a workspace member outside the private channel sees only public objects",
			user: w.outsider, ws: w.ws, params: url.Values{},
			want: sorted("message:"+w.pubMsg, "file:"+w.pubFile),
		},
		{
			name: "type narrows to files",
			user: w.insider, ws: w.ws, params: url.Values{"type": {"file"}},
			want: sorted("file:"+w.pubFile, "file:"+w.privFile, "file:"+w.unattachedFile),
		},
		{
			name: "type accepts a list",
			user: w.outsider, ws: w.ws, params: url.Values{"type": {"message,file"}},
			want: sorted("message:"+w.pubMsg, "file:"+w.pubFile),
		},
		{
			name: "channel narrows to one channel, across types",
			user: w.insider, ws: w.ws, params: url.Values{"channel": {w.priv}},
			want: sorted("message:"+w.privMsg, "file:"+w.privFile),
		},
		{
			name: "from narrows to one author",
			user: w.outsider, ws: w.ws, params: url.Values{"from": {w.outsider}},
			want: []string{},
		},
		{
			// The other tenant's own workspace contains nothing; the point is
			// that searching it does not reach into this one.
			name: "another tenant searching their own workspace finds nothing",
			user: w.stranger, ws: w.otherWS, params: url.Values{},
			want: []string{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := w.hits(t, tt.user, tt.ws, tt.params)
			if len(got) != len(tt.want) {
				t.Fatalf("hits = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("hits = %v, want %v", got, tt.want)
				}
			}
		})
	}
}

func TestSearchRejects(t *testing.T) {
	w := seedWorld(t)

	tests := []struct {
		name   string
		user   string
		ws     string
		params url.Values
		want   int
	}{
		{"another tenant cannot search this workspace at all", w.stranger, w.ws,
			url.Values{"q": {w.needle}}, http.StatusForbidden},
		{"naming a channel of another workspace is refused", w.stranger, w.otherWS,
			url.Values{"q": {w.needle}, "channel": {w.priv}}, http.StatusForbidden},
		{"a member cannot name a channel they may not read", w.outsider, w.ws,
			url.Values{"q": {w.needle}, "channel": {w.priv}}, http.StatusForbidden},
		{"a channel id that is not a uuid is refused", w.outsider, w.ws,
			url.Values{"q": {w.needle}, "channel": {"general"}}, http.StatusForbidden},
		{"q is required", w.insider, w.ws, url.Values{}, http.StatusBadRequest},
		{"an unknown type is rejected, not ignored", w.insider, w.ws,
			url.Values{"q": {w.needle}, "type": {"message,everything"}}, http.StatusBadRequest},
		{"from must be a user id", w.insider, w.ws,
			url.Values{"q": {w.needle}, "from": {"me"}}, http.StatusBadRequest},
		{"limit is bounded", w.insider, w.ws,
			url.Values{"q": {w.needle}, "limit": {"1000"}}, http.StatusBadRequest},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, body := w.get(t, tt.user, tt.ws, tt.params)
			if code != tt.want {
				t.Fatalf("status = %d, want %d (body %s)", code, tt.want, body)
			}
			if strings.Contains(body, w.privMsg) || strings.Contains(body, w.privFile) {
				t.Fatalf("error response leaked private ids: %s", body)
			}
		})
	}
}

// An object's key set names the channels and users it is shared with. It is
// stored, it is filtered on, and it never leaves the server.
func TestSearchResponseNeverCarriesTheACL(t *testing.T) {
	w := seedWorld(t)
	code, body := w.get(t, w.insider, w.ws, url.Values{"q": {w.needle}})
	if code != http.StatusOK {
		t.Fatalf("status %d: %s", code, body)
	}
	for _, forbidden := range []string{"acl", "doc_id", ChannelKey(w.priv), UserKey(w.insider)} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("response contains %q: %s", forbidden, body)
		}
	}
	// ...while still carrying what a client needs to render and route a hit.
	for _, required := range []string{w.pubMsg, `"type":"message"`, `"channel_id":"` + w.pub + `"`, `"title"`} {
		if !strings.Contains(body, required) {
			t.Fatalf("response is missing %q: %s", required, body)
		}
	}
}

// The retention purge hard-deletes rows out of Postgres and calls
// DeleteMessages; if that stopped working, purged content would stay fully
// searchable and the retention policy would be decorative.
func TestDeleteMessagesRemovesPurgedContent(t *testing.T) {
	w := seedWorld(t)
	svc := testService(t)

	before := w.hits(t, w.insider, w.ws, url.Values{"type": {"message"}})
	if len(before) != 2 {
		t.Fatalf("fixture is broken: %v", before)
	}

	if err := svc.DeleteMessages(t.Context(), []string{w.pubMsg, w.privMsg}); err != nil {
		t.Fatalf("DeleteMessages: %v", err)
	}

	// DeleteMessages deliberately does not await its task — retention calls it
	// while holding row locks — so the assertion polls instead.
	deadline := time.Now().Add(20 * time.Second)
	var got []string
	for time.Now().Before(deadline) {
		got = w.hits(t, w.insider, w.ws, url.Values{"type": {"message"}})
		if len(got) == 0 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("purged messages are still searchable: %v", got)
}

// AccessKeys is the query side of the ACL model; an empty result from it is the
// thing that makes an unreadable workspace return nothing instead of everything.
func TestAccessKeys(t *testing.T) {
	w := seedWorld(t)
	pool := testDB(t)
	az := authz.New(pool)

	tests := []struct {
		name string
		ws   string
		user string
		want []string
	}{
		{"member of a channel gets the workspace, user and both channel keys",
			w.ws, w.insider,
			sorted(WorkspaceKey(w.ws), UserKey(w.insider), ChannelKey(w.pub), ChannelKey(w.priv))},
		{"member outside the private channel does not get its key",
			w.ws, w.outsider,
			sorted(WorkspaceKey(w.ws), UserKey(w.outsider), ChannelKey(w.pub))},
		{"a non-member gets no keys at all", w.ws, w.stranger, []string{}},
		{"an unknown workspace grants nothing", uuid.NewString(), w.insider, []string{}},
		{"an empty user id grants nothing", w.ws, "", []string{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := AccessKeys(t.Context(), az, tt.ws, tt.user)
			if err != nil {
				t.Fatalf("AccessKeys: %v", err)
			}
			sort.Strings(got)
			if len(got) != len(tt.want) {
				t.Fatalf("keys = %v, want %v", got, tt.want)
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Fatalf("keys = %v, want %v", got, tt.want)
				}
			}
		})
	}
}
