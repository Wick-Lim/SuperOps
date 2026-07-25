package ws

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"
)

const testDocumentID = "1b7f2a90-5c3e-4f21-8a6d-0e9c4b5a7d31"

// stubRoom stands in for the collaboration service. What is under test here is
// the socket half of the contract — who is let into a room, what a revocation
// does to a live connection, and which budget a collaboration frame is charged
// to. The real service is exercised end-to-end in internal/collab.
type stubRoom struct {
	mu sync.Mutex

	access    RoomAccess
	joinErr   error
	recheck   RoomAccess
	recheckOK bool
	recheckNo error

	updates   [][]byte
	awareness [][]byte
	updateErr error
}

func (s *stubRoom) Join(context.Context, string, string) (RoomAccess, error) {
	return s.access, s.joinErr
}

func (s *stubRoom) Recheck(context.Context, string, string) (RoomAccess, error) {
	if !s.recheckOK {
		return RoomAccess{}, s.recheckNo
	}
	return s.recheck, nil
}

func (s *stubRoom) Update(_ context.Context, _, _, _ string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.updateErr != nil {
		return s.updateErr
	}
	s.updates = append(s.updates, payload)
	return nil
}

func (s *stubRoom) Awareness(_ context.Context, _, _, _ string, payload []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.awareness = append(s.awareness, payload)
	return nil
}

func (s *stubRoom) counts() (updates, awareness int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.updates), len(s.awareness)
}

func newRoomClient(hub *Hub, room RoomHandler) *Client {
	return NewClient(context.Background(), hub, nil, "u1", []string{"ws-1"}, nil, nil, room, testLogger())
}

func TestCollabFrames(t *testing.T) {
	tests := []struct {
		name        string
		room        *stubRoom
		joined      bool // pre-join the room
		canWrite    bool
		msgType     string
		data        interface{}
		wantTypes   []string
		wantErrCode string
		wantUpdates int
		wantAware   int
		wantInRoom  bool
	}{
		{
			name:       "join grants write",
			room:       &stubRoom{access: RoomAccess{HeadSeq: 7, CanWrite: true}},
			msgType:    TypeCollabJoin,
			data:       CollabDocumentData{DocumentID: testDocumentID},
			wantTypes:  []string{TypeCollabJoined},
			wantInRoom: true,
		},
		{
			name:        "join refused",
			room:        &stubRoom{joinErr: ErrRoomForbidden},
			msgType:     TypeCollabJoin,
			data:        CollabDocumentData{DocumentID: testDocumentID},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
		},
		{
			name:        "join of an unknown document",
			room:        &stubRoom{joinErr: ErrRoomNotFound},
			msgType:     TypeCollabJoin,
			data:        CollabDocumentData{DocumentID: testDocumentID},
			wantTypes:   []string{TypeError},
			wantErrCode: "NOT_FOUND",
		},
		{
			// A failure to decide is not a denial: reporting FORBIDDEN would
			// make a database blip look like a permission change to the editor.
			name:        "join could not be decided",
			room:        &stubRoom{joinErr: context.DeadlineExceeded},
			msgType:     TypeCollabJoin,
			data:        CollabDocumentData{DocumentID: testDocumentID},
			wantTypes:   []string{TypeError},
			wantErrCode: "INTERNAL_ERROR",
		},
		{
			name:        "join with a non-UUID document id",
			room:        &stubRoom{},
			msgType:     TypeCollabJoin,
			data:        map[string]string{"document_id": "../../etc"},
			wantTypes:   []string{TypeError},
			wantErrCode: "BAD_REQUEST",
		},
		{
			name:        "update without joining",
			room:        &stubRoom{},
			msgType:     TypeCollabUpdate,
			data:        CollabUpdateData{DocumentID: testDocumentID, Update: []byte("x")},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
		},
		{
			name:        "update while read-only",
			room:        &stubRoom{},
			joined:      true,
			canWrite:    false,
			msgType:     TypeCollabUpdate,
			data:        CollabUpdateData{DocumentID: testDocumentID, Update: []byte("x")},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
			wantInRoom:  true,
		},
		{
			name:        "update accepted",
			room:        &stubRoom{},
			joined:      true,
			canWrite:    true,
			msgType:     TypeCollabUpdate,
			data:        CollabUpdateData{DocumentID: testDocumentID, Update: []byte("hello")},
			wantUpdates: 1,
			wantInRoom:  true,
		},
		{
			name:        "update over the socket budget",
			room:        &stubRoom{},
			joined:      true,
			canWrite:    true,
			msgType:     TypeCollabUpdate,
			data:        CollabUpdateData{DocumentID: testDocumentID, Update: make([]byte, maxCollabPayloadBytes+1)},
			wantTypes:   []string{TypeError},
			wantErrCode: "PAYLOAD_TOO_LARGE",
			wantInRoom:  true,
		},
		{
			name:        "empty update",
			room:        &stubRoom{},
			joined:      true,
			canWrite:    true,
			msgType:     TypeCollabUpdate,
			data:        CollabUpdateData{DocumentID: testDocumentID},
			wantTypes:   []string{TypeError},
			wantErrCode: "BAD_REQUEST",
			wantInRoom:  true,
		},
		{
			// Awareness needs read access only — a viewer's cursor is as
			// visible as an editor's.
			name:       "awareness while read-only",
			room:       &stubRoom{},
			joined:     true,
			canWrite:   false,
			msgType:    TypeCollabAwareness,
			data:       CollabAwarenessData{DocumentID: testDocumentID, State: []byte("cursor")},
			wantAware:  1,
			wantInRoom: true,
		},
		{
			name:        "awareness without joining",
			room:        &stubRoom{},
			msgType:     TypeCollabAwareness,
			data:        CollabAwarenessData{DocumentID: testDocumentID, State: []byte("cursor")},
			wantTypes:   []string{TypeError},
			wantErrCode: "FORBIDDEN",
		},
		{
			name:      "leave",
			room:      &stubRoom{},
			joined:    true,
			canWrite:  true,
			msgType:   TypeCollabLeave,
			data:      CollabDocumentData{DocumentID: testDocumentID},
			wantTypes: []string{TypeCollabLeft},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(testLogger())
			c := newRoomClient(hub, tt.room)
			if tt.joined {
				c.JoinRoom(testDocumentID, tt.canWrite)
			}

			c.handleMessage(context.Background(), inbound(t, tt.msgType, tt.data))

			frames := drain(t, c)
			if len(frames) != len(tt.wantTypes) {
				t.Fatalf("got %d frames %v, want %v", len(frames), frames, tt.wantTypes)
			}
			for i, want := range tt.wantTypes {
				if frames[i].Type != want {
					t.Errorf("frame %d type = %q (%s), want %q", i, frames[i].Type, frames[i].Data, want)
				}
			}
			if tt.wantErrCode != "" && errCode(frames[0]) != tt.wantErrCode {
				t.Errorf("error code = %q, want %q", errCode(frames[0]), tt.wantErrCode)
			}

			updates, awareness := tt.room.counts()
			if updates != tt.wantUpdates {
				t.Errorf("persisted %d updates, want %d", updates, tt.wantUpdates)
			}
			if awareness != tt.wantAware {
				t.Errorf("relayed %d awareness states, want %d", awareness, tt.wantAware)
			}
			if c.InRoom(testDocumentID) != tt.wantInRoom {
				t.Errorf("in room = %v, want %v", c.InRoom(testDocumentID), tt.wantInRoom)
			}
		})
	}
}

// TestCollabJoinWithNoHandler: a server without the collaboration layer wired
// must say so rather than look like a dropped frame.
func TestCollabJoinWithNoHandler(t *testing.T) {
	hub := NewHub(testLogger())
	c := newRoomClient(hub, nil)

	c.handleMessage(context.Background(), inbound(t, TypeCollabJoin, CollabDocumentData{DocumentID: testDocumentID}))

	frames := drain(t, c)
	if len(frames) != 1 || errCode(frames[0]) != "UNSUPPORTED" {
		t.Fatalf("frames = %v, want a single UNSUPPORTED error", frames)
	}
	if c.InRoom(testDocumentID) {
		t.Fatal("joined a room with no collaboration layer wired")
	}
}

func TestRecheckRooms(t *testing.T) {
	tests := []struct {
		name       string
		room       *stubRoom
		wantInRoom bool
		wantWrite  bool
		wantFrames int
	}{
		{
			name:       "access withdrawn",
			room:       &stubRoom{recheckNo: ErrRoomForbidden},
			wantFrames: 1,
		},
		{
			name:       "document deleted",
			room:       &stubRoom{recheckNo: ErrRoomNotFound},
			wantFrames: 1,
		},
		{
			// A database blip must not revoke a legitimate session.
			name:       "undecidable",
			room:       &stubRoom{recheckNo: context.DeadlineExceeded},
			wantInRoom: true,
			wantWrite:  true,
		},
		{
			name:       "demoted to read-only",
			room:       &stubRoom{recheckOK: true, recheck: RoomAccess{CanWrite: false}},
			wantInRoom: true,
		},
		{
			name:       "still a writer",
			room:       &stubRoom{recheckOK: true, recheck: RoomAccess{CanWrite: true}},
			wantInRoom: true,
			wantWrite:  true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hub := NewHub(testLogger())
			c := newRoomClient(hub, tt.room)
			c.JoinRoom(testDocumentID, true)

			c.recheckRooms()

			if got := c.InRoom(testDocumentID); got != tt.wantInRoom {
				t.Fatalf("in room = %v, want %v", got, tt.wantInRoom)
			}
			if got := c.CanWriteRoom(testDocumentID); got != tt.wantWrite {
				t.Errorf("can write = %v, want %v", got, tt.wantWrite)
			}

			frames := drain(t, c)
			if len(frames) != tt.wantFrames {
				t.Fatalf("got %d frames %v, want %d", len(frames), frames, tt.wantFrames)
			}
			if tt.wantFrames > 0 {
				if frames[0].Type != TypeCollabLeft {
					t.Fatalf("frame type = %q, want %q", frames[0].Type, TypeCollabLeft)
				}
				var payload struct {
					Reason string `json:"reason"`
				}
				if err := json.Unmarshal(frames[0].Data, &payload); err != nil {
					t.Fatalf("decode: %v", err)
				}
				if payload.Reason != ReasonRevoked {
					t.Errorf("reason = %q, want %q", payload.Reason, ReasonRevoked)
				}
			}
		})
	}
}

// TestRevokeRoomStopsDelivery is the revocation the hub owns: the room map is
// what delivery is gated on, so dropping it is what actually stops the frames.
func TestRevokeRoomStopsDelivery(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()
	defer hub.Shutdown()

	victim := newTestClient(hub, "u1")
	bystander := newTestClient(hub, "u2")
	hub.register <- victim
	hub.register <- bystander
	waitFor(t, "both registered", func() bool { return hub.ConnectionCount() == 2 })

	victim.JoinRoom(testDocumentID, true)
	bystander.JoinRoom(testDocumentID, true)

	hub.BroadcastToRoomLocal(testDocumentID, TypeCollabUpdate, map[string]string{"document_id": testDocumentID})
	waitFor(t, "both to receive the update", func() bool {
		return len(victim.send) == 1 && len(bystander.send) == 1
	})

	hub.RevokeRoom("u1", testDocumentID)
	if victim.InRoom(testDocumentID) {
		t.Fatal("room membership survived revocation")
	}
	if !bystander.InRoom(testDocumentID) {
		t.Fatal("revoking one user removed another from the room")
	}

	hub.BroadcastToRoomLocal(testDocumentID, TypeCollabUpdate, map[string]string{"document_id": testDocumentID})
	// The victim has its update, its collab.left, and nothing else.
	frames := drain(t, victim)
	if len(frames) != 2 || frames[1].Type != TypeCollabLeft {
		t.Fatalf("victim frames = %v, want the first update then a collab.left", frames)
	}
	if got := len(drain(t, bystander)); got != 2 {
		t.Fatalf("bystander received %d frames, want 2", got)
	}
}

// TestSendToRoomLeaderPicksExactlyOne: asking every client for a snapshot would
// have the whole room upload a full document copy at once.
func TestSendToRoomLeaderPicksExactlyOne(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()
	defer hub.Shutdown()

	clients := make([]*Client, 4)
	for i := range clients {
		clients[i] = newTestClient(hub, "u1")
		hub.register <- clients[i]
		clients[i].JoinRoom(testDocumentID, true)
	}
	waitFor(t, "all registered", func() bool { return hub.ConnectionCount() == 4 })

	if !hub.SendToRoomLeader(testDocumentID, TypeCollabCompact, map[string]string{"document_id": testDocumentID}) {
		t.Fatal("SendToRoomLeader reported no local member of a room with four")
	}

	asked := 0
	for _, c := range clients {
		asked += len(c.send)
	}
	if asked != 1 {
		t.Fatalf("%d clients were asked to compact, want exactly 1", asked)
	}

	// Deterministic: the same room elects the same leader.
	hub.SendToRoomLeader(testDocumentID, TypeCollabCompact, map[string]string{"document_id": testDocumentID})
	for _, c := range clients {
		if n := len(c.send); n != 0 && n != 2 {
			t.Fatalf("leader election is not stable: a client holds %d frames", n)
		}
	}

	if hub.SendToRoomLeader("11111111-2222-3333-4444-555555555555", TypeCollabCompact, nil) {
		t.Fatal("SendToRoomLeader claimed to deliver into an empty room")
	}
}

// TestCollabFramesUseTheirOwnBudget: an editing session must not consume the
// 5/s chat budget, or a person typing disconnects everything else they do.
func TestCollabFramesUseTheirOwnBudget(t *testing.T) {
	hub := NewHub(testLogger())
	c := newRoomClient(hub, &stubRoom{})
	now := time.Now()

	// Spend far more than the general burst on collaboration frames.
	for i := 0; i < inboundBurst*2; i++ {
		if allowed, _ := c.chargeInbound(c.collabLimiter); !allowed {
			t.Fatalf("collaboration budget exhausted after %d frames, well inside its burst of %d", i, collabBurst)
		}
	}

	if !c.limiter.allow(now) {
		t.Fatal("collaboration frames drained the general frame budget")
	}

	// A join is a database round-trip, not a keystroke, so it must not be on
	// the high-rate budget.
	for _, tt := range []struct {
		msgType string
		want    bool
	}{
		{TypeCollabUpdate, true},
		{TypeCollabAwareness, true},
		{TypeCollabJoin, false},
		{TypeCollabLeave, false},
		{TypeSubscribe, false},
	} {
		if got := isCollabStreamType(tt.msgType); got != tt.want {
			t.Errorf("isCollabStreamType(%q) = %v, want %v", tt.msgType, got, tt.want)
		}
	}
}
