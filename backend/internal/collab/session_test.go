package collab

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/Wick-Lim/SuperOps/backend/internal/auth"
	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
)

// These tests drive the collaboration layer the way a client does: real
// WebSocket connections to a real hub, with a real Postgres behind them. A
// stubbed hub would test that the service calls a broadcast method, which is
// not the question — the question is whether two editors converge, whether a
// reconnecting one is given exactly what it missed, and whether a revoked one
// stops receiving keystrokes.

const testJWTSecret = "collab-test-secret-0123456789abcdef"

type server struct {
	*fixture
	http *httptest.Server
	jwt  *auth.JWTManager
}

// serve exposes the fixture's hub over a real HTTP server.
func (f *fixture) serve(t *testing.T) *server {
	t.Helper()
	return serveHub(t, f, f.hub, f.svc)
}

func serveHub(t *testing.T, f *fixture, hub *ws.Hub, svc *Service) *server {
	t.Helper()

	jwtMgr := auth.NewJWTManager(testJWTSecret, time.Hour)
	handler := ws.NewWSHandler(
		hub,
		jwtMgr,
		nil, // presence is Redis-backed and irrelevant to a collaboration room
		nil, // channel membership: fails closed, and no test subscribes to one
		func(context.Context, string) ([]string, error) { return []string{f.workspaceID}, nil },
		nil,
		testLogger(),
		ws.WithRoomHandler(svc),
	)

	mux := http.NewServeMux()
	handler.RegisterRoutes(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)

	return &server{fixture: f, http: srv, jwt: jwtMgr}
}

// socket is one connected client. Frames are drained by a goroutine so a test
// can assert on what arrives without deadlocking against what it is sending.
type socket struct {
	conn   *websocket.Conn
	frames chan ws.OutboundMessage
}

func (s *server) dial(t *testing.T, userID string) *socket {
	t.Helper()

	token, err := s.jwt.Generate(userID)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	url := "ws" + strings.TrimPrefix(s.http.URL, "http") + "/api/v1/ws?token=" + token
	conn, _, err := websocket.Dial(ctx, url, nil)
	if err != nil {
		t.Fatalf("dial websocket: %v", err)
	}

	sock := &socket{conn: conn, frames: make(chan ws.OutboundMessage, 256)}
	go func() {
		defer close(sock.frames)
		for {
			_, data, err := conn.Read(context.Background())
			if err != nil {
				return
			}
			var msg ws.OutboundMessage
			if err := json.Unmarshal(data, &msg); err != nil {
				return
			}
			sock.frames <- msg
		}
	}()

	t.Cleanup(func() { _ = conn.Close(websocket.StatusNormalClosure, "") })

	// The hello frame is the handshake; every test starts after it.
	if got := sock.next(t); got.Type != ws.TypeHello {
		t.Fatalf("first frame = %q, want %q", got.Type, ws.TypeHello)
	}
	return sock
}

func (s *socket) send(t *testing.T, msgType string, data interface{}) {
	t.Helper()
	raw, err := json.Marshal(data)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", msgType, err)
	}
	body, err := json.Marshal(ws.InboundMessage{Type: msgType, Data: raw})
	if err != nil {
		t.Fatalf("marshal %s frame: %v", msgType, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.conn.Write(ctx, websocket.MessageText, body); err != nil {
		t.Fatalf("write %s: %v", msgType, err)
	}
}

func (s *socket) next(t *testing.T) ws.OutboundMessage {
	t.Helper()
	select {
	case msg, ok := <-s.frames:
		if !ok {
			t.Fatal("connection closed while waiting for a frame")
		}
		return msg
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for a frame")
		return ws.OutboundMessage{}
	}
}

// expect reads the next frame and asserts its type, which is what makes an
// unexpected error frame a readable failure instead of a timeout.
func (s *socket) expect(t *testing.T, msgType string) ws.OutboundMessage {
	t.Helper()
	msg := s.next(t)
	if msg.Type != msgType {
		t.Fatalf("frame type = %q (%s), want %q", msg.Type, msg.Data, msgType)
	}
	return msg
}

// silent asserts nothing arrives. It is the only way to test that a revoked
// session actually stopped receiving.
func (s *socket) silent(t *testing.T, d time.Duration) {
	t.Helper()
	select {
	case msg, ok := <-s.frames:
		if ok {
			t.Fatalf("expected silence, got a %q frame: %s", msg.Type, msg.Data)
		}
	case <-time.After(d):
	}
}

func (s *socket) close() {
	_ = s.conn.Close(websocket.StatusNormalClosure, "")
}

func join(t *testing.T, s *socket, documentID string) ws.OutboundMessage {
	t.Helper()
	s.send(t, ws.TypeCollabJoin, map[string]string{"document_id": documentID})
	return s.expect(t, ws.TypeCollabJoined)
}

func decode(t *testing.T, msg ws.OutboundMessage, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(msg.Data, v); err != nil {
		t.Fatalf("decode %s payload: %v", msg.Type, err)
	}
}

// updateFrame is the shape of an outbound collab.update, decoded the way a
// client would decode it.
type updateFrame struct {
	DocumentID string `json:"document_id"`
	Seq        int64  `json:"seq"`
	ActorID    string `json:"actor_id"`
	OriginConn string `json:"origin_conn"`
	Update     []byte `json:"update"`
}

// TestTwoClientsConverge: two editors in one document each see every update,
// including their own echo, with the sequence numbers the log assigned.
func TestTwoClientsConverge(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	a := srv.dial(t, f.owner)
	b := srv.dial(t, f.member)

	joined := join(t, a, doc.ID)
	var access struct {
		HeadSeq  int64 `json:"head_seq"`
		CanWrite bool  `json:"can_write"`
	}
	decode(t, joined, &access)
	if access.HeadSeq != 0 || !access.CanWrite {
		t.Fatalf("owner joined with head %d can_write %v, want 0/true", access.HeadSeq, access.CanWrite)
	}
	join(t, b, doc.ID)

	a.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("from-a"),
	})

	for name, s := range map[string]*socket{"sender": a, "peer": b} {
		var got updateFrame
		decode(t, s.expect(t, ws.TypeCollabUpdate), &got)
		if got.Seq != 1 || string(got.Update) != "from-a" || got.ActorID != f.owner {
			t.Fatalf("%s got seq %d %q from %s, want seq 1 %q from %s",
				name, got.Seq, got.Update, got.ActorID, "from-a", f.owner)
		}
		if got.DocumentID != doc.ID {
			t.Fatalf("%s got an update for document %s, want %s", name, got.DocumentID, doc.ID)
		}
	}

	b.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("from-b"),
	})
	for name, s := range map[string]*socket{"sender": b, "peer": a} {
		var got updateFrame
		decode(t, s.expect(t, ws.TypeCollabUpdate), &got)
		if got.Seq != 2 || string(got.Update) != "from-b" {
			t.Fatalf("%s got seq %d %q, want seq 2 %q", name, got.Seq, got.Update, "from-b")
		}
	}

	// Both edits are durable and in order — a third client loading from scratch
	// converges on the same document.
	state, err := f.svc.Load(context.Background(), doc.ID, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if len(state.Updates) != 2 ||
		string(state.Updates[0].Payload) != "from-a" ||
		string(state.Updates[1].Payload) != "from-b" {
		t.Fatalf("log = %v, want from-a then from-b", state.Updates)
	}
}

// TestAwarenessIsEphemeral: a cursor reaches the other editor and never reaches
// Postgres.
func TestAwarenessIsEphemeral(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	a := srv.dial(t, f.owner)
	b := srv.dial(t, f.member)
	join(t, a, doc.ID)
	join(t, b, doc.ID)

	a.send(t, ws.TypeCollabAwareness, map[string]interface{}{
		"document_id": doc.ID,
		"state":       []byte("cursor:12"),
	})

	var got struct {
		ActorID string `json:"actor_id"`
		State   []byte `json:"state"`
	}
	decode(t, b.expect(t, ws.TypeCollabAwareness), &got)
	if string(got.State) != "cursor:12" || got.ActorID != f.owner {
		t.Fatalf("awareness = %q from %s, want %q from %s", got.State, got.ActorID, "cursor:12", f.owner)
	}

	var updates, head int64
	if err := f.pool.QueryRow(context.Background(),
		`SELECT (SELECT count(*) FROM collab_updates WHERE document_id = $1),
		        (SELECT head_seq FROM collab_documents WHERE id = $1)`, doc.ID,
	).Scan(&updates, &head); err != nil {
		t.Fatalf("count: %v", err)
	}
	if updates != 0 || head != 0 {
		t.Fatalf("awareness wrote to Postgres: %d updates, head %d", updates, head)
	}
}

// TestReconnectReceivesMissedUpdates: a client that was offline for part of the
// session catches up on exactly what it missed, and nothing else.
func TestReconnectReceivesMissedUpdates(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)
	ctx := context.Background()

	a := srv.dial(t, f.owner)
	b := srv.dial(t, f.member)
	join(t, a, doc.ID)
	join(t, b, doc.ID)

	// A sends two updates and notes its watermark, then drops off.
	var watermark int64
	for i := 0; i < 2; i++ {
		a.send(t, ws.TypeCollabUpdate, map[string]interface{}{
			"document_id": doc.ID,
			"update":      []byte{byte('a' + i)},
		})
		var got updateFrame
		decode(t, a.expect(t, ws.TypeCollabUpdate), &got)
		watermark = got.Seq
		b.expect(t, ws.TypeCollabUpdate)
	}
	a.close()

	// B keeps editing while A is away.
	for i := 0; i < 3; i++ {
		b.send(t, ws.TypeCollabUpdate, map[string]interface{}{
			"document_id": doc.ID,
			"update":      []byte{byte('x' + i)},
		})
		b.expect(t, ws.TypeCollabUpdate)
	}

	// A comes back: join tells it the head, and the state endpoint gives it the
	// gap.
	a2 := srv.dial(t, f.owner)
	var access struct {
		HeadSeq int64 `json:"head_seq"`
	}
	decode(t, join(t, a2, doc.ID), &access)
	if access.HeadSeq != 5 {
		t.Fatalf("head_seq on rejoin = %d, want 5", access.HeadSeq)
	}
	if access.HeadSeq <= watermark {
		t.Fatal("a reconnecting client with missed updates was told it was current")
	}

	state, err := f.svc.Load(ctx, doc.ID, watermark)
	if err != nil {
		t.Fatalf("load since %d: %v", watermark, err)
	}
	if state.Snapshot != nil {
		t.Fatal("an uncompacted document sent a snapshot to a short reconnect")
	}
	if len(state.Updates) != 3 {
		t.Fatalf("caught up with %d updates, want 3", len(state.Updates))
	}
	for i, u := range state.Updates {
		if u.Seq != watermark+int64(i)+1 {
			t.Fatalf("gap update %d has seq %d, want %d", i, u.Seq, watermark+int64(i)+1)
		}
		if string(u.Payload) != string([]byte{byte('x' + i)}) {
			t.Fatalf("gap update %d = %q, want %q", i, u.Payload, []byte{byte('x' + i)})
		}
	}
	if state.ThroughSeq != 5 || state.HasMore {
		t.Fatalf("through_seq = %d has_more = %v, want 5/false", state.ThroughSeq, state.HasMore)
	}

	// And it keeps receiving live updates on the new socket.
	b.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("live"),
	})
	var got updateFrame
	decode(t, a2.expect(t, ws.TypeCollabUpdate), &got)
	if got.Seq != 6 {
		t.Fatalf("live update after reconnect = seq %d, want 6", got.Seq)
	}
}

// TestRevocationCutsSessionOff: access withdrawn mid-session stops delivery
// immediately, in both directions.
func TestRevocationCutsSessionOff(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	victim := srv.dial(t, f.member)
	other := srv.dial(t, f.owner)
	join(t, victim, doc.ID)
	join(t, other, doc.ID)

	// Proof the room works before it is cut.
	other.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("before"),
	})
	victim.expect(t, ws.TypeCollabUpdate)
	other.expect(t, ws.TypeCollabUpdate)

	f.svc.RevokeAccess(f.member, doc.ID)

	var left struct {
		DocumentID string `json:"document_id"`
		Reason     string `json:"reason"`
	}
	decode(t, victim.expect(t, ws.TypeCollabLeft), &left)
	if left.DocumentID != doc.ID || left.Reason != "revoked" {
		t.Fatalf("left frame = %+v, want document %s revoked", left, doc.ID)
	}

	// Reads stop.
	other.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("after"),
	})
	other.expect(t, ws.TypeCollabUpdate)
	victim.silent(t, 300*time.Millisecond)

	// Writes stop too — a revoked client must not be able to keep appending
	// just because it still holds the socket.
	victim.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("sneaky"),
	})
	var errFrame struct {
		Code string `json:"code"`
	}
	decode(t, victim.expect(t, ws.TypeError), &errFrame)
	if errFrame.Code != "FORBIDDEN" {
		t.Fatalf("revoked write error = %q, want FORBIDDEN", errFrame.Code)
	}

	state, err := f.svc.Load(context.Background(), doc.ID, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	for _, u := range state.Updates {
		if string(u.Payload) == "sneaky" {
			t.Fatal("a revoked client's update reached the log")
		}
	}
}

// TestJoinAuthorization covers the three answers a join can have.
func TestJoinAuthorization(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	t.Run("stranger is refused", func(t *testing.T) {
		s := srv.dial(t, f.stranger)
		s.send(t, ws.TypeCollabJoin, map[string]string{"document_id": doc.ID})
		var got struct {
			Code string `json:"code"`
		}
		decode(t, s.expect(t, ws.TypeError), &got)
		if got.Code != "FORBIDDEN" {
			t.Fatalf("error code = %q, want FORBIDDEN", got.Code)
		}
	})

	t.Run("guest joins read-only", func(t *testing.T) {
		s := srv.dial(t, f.guest)
		var access struct {
			CanWrite bool `json:"can_write"`
		}
		decode(t, join(t, s, doc.ID), &access)
		if access.CanWrite {
			t.Fatal("a guest was granted write access")
		}

		s.send(t, ws.TypeCollabUpdate, map[string]interface{}{
			"document_id": doc.ID,
			"update":      []byte("nope"),
		})
		var got struct {
			Code string `json:"code"`
		}
		decode(t, s.expect(t, ws.TypeError), &got)
		if got.Code != "FORBIDDEN" {
			t.Fatalf("guest write error = %q, want FORBIDDEN", got.Code)
		}
	})

	t.Run("unknown document", func(t *testing.T) {
		s := srv.dial(t, f.owner)
		s.send(t, ws.TypeCollabJoin, map[string]string{"document_id": authz.RoleOwner})
		var got struct {
			Code string `json:"code"`
		}
		decode(t, s.expect(t, ws.TypeError), &got)
		if got.Code != "BAD_REQUEST" {
			t.Fatalf("error code = %q, want BAD_REQUEST for a non-UUID document id", got.Code)
		}
	})
}

// TestUpdateWithoutJoin: the room membership is the authorization, so an update
// for a document the connection never joined must be refused even though the
// user could have joined it.
func TestUpdateWithoutJoin(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	s := srv.dial(t, f.owner)
	s.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("x"),
	})
	var got struct {
		Code string `json:"code"`
	}
	decode(t, s.expect(t, ws.TypeError), &got)
	if got.Code != "FORBIDDEN" {
		t.Fatalf("error code = %q, want FORBIDDEN", got.Code)
	}
}

// TestOversizedUpdateIsRefused: the socket's bound is what stops one client
// pushing arbitrary memory through the server.
func TestOversizedUpdateIsRefused(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)

	s := srv.dial(t, f.owner)
	join(t, s, doc.ID)

	// Just over the socket's decoded-payload budget, and small enough that the
	// frame itself still arrives.
	s.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      make([]byte, 33<<10),
	})
	var got struct {
		Code string `json:"code"`
	}
	decode(t, s.expect(t, ws.TypeError), &got)
	if got.Code != "PAYLOAD_TOO_LARGE" {
		t.Fatalf("error code = %q, want PAYLOAD_TOO_LARGE", got.Code)
	}
}

// TestCompactionIsRequestedFromOneClient: the server cannot snapshot by itself,
// so the only thing that bounds a long-lived log is this request reaching a
// client.
func TestCompactionIsRequestedFromOneClient(t *testing.T) {
	f := newFixture(t)
	srv := f.serve(t)
	doc := f.newDocument(t)
	f.svc.SetCompactionThreshold(3)

	a := srv.dial(t, f.owner)
	b := srv.dial(t, f.member)
	join(t, a, doc.ID)
	join(t, b, doc.ID)

	for i := 0; i < 3; i++ {
		a.send(t, ws.TypeCollabUpdate, map[string]interface{}{
			"document_id": doc.ID,
			"update":      []byte{byte(i)},
		})
	}

	// Exactly one of the two is asked; which one is deterministic but depends
	// on generated connection ids, so the assertion is on the count.
	asked := 0
	var request struct {
		DocumentID string `json:"document_id"`
		HeadSeq    int64  `json:"head_seq"`
	}
	for _, s := range []*socket{a, b} {
		deadline := time.After(750 * time.Millisecond)
		for done := false; !done; {
			select {
			case msg, ok := <-s.frames:
				if !ok {
					done = true
					break
				}
				if msg.Type == ws.TypeCollabCompact {
					asked++
					decode(t, msg, &request)
				}
			case <-deadline:
				done = true
			}
		}
	}
	if asked != 1 {
		t.Fatalf("%d clients were asked for a snapshot, want exactly 1", asked)
	}
	if request.DocumentID != doc.ID || request.HeadSeq != 3 {
		t.Fatalf("compaction request = %+v, want document %s at head 3", request, doc.ID)
	}

	// The client answers, and the log is compacted.
	compacted, err := f.svc.SaveSnapshot(context.Background(), doc.ID, request.HeadSeq, f.owner, []byte("client-state"))
	if err != nil {
		t.Fatalf("save snapshot: %v", err)
	}
	if compacted != 3 {
		t.Fatalf("compacted %d updates, want 3", compacted)
	}

	state, err := f.svc.Load(context.Background(), doc.ID, 0)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if string(state.Snapshot) != "client-state" || len(state.Updates) != 0 {
		t.Fatalf("after compaction: snapshot %q with %d updates, want the snapshot alone",
			state.Snapshot, len(state.Updates))
	}
}
