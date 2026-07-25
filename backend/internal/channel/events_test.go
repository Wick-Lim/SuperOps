package channel

import (
	"encoding/json"
	"testing"
	"time"
)

// The event relay (internal/ws/relay.go) routes every domain event on exact
// top-level JSON keys and silently drops a payload that does not carry them —
// which is how these events shipped dead. These are the shapes it decodes into;
// they are mirrored rather than imported so a change to either side has to be a
// deliberate one.
type relayChannelTarget struct {
	ChannelID string `json:"channel_id"`
}

type relayCreatedTarget struct {
	WorkspaceID string   `json:"workspace_id"`
	Type        string   `json:"type"`
	MemberIDs   []string `json:"member_ids"`
}

type relayUserTarget struct {
	UserID string `json:"user_id"`
}

// recordedEvent is one publish captured from a handler.
type recordedEvent struct {
	method      string
	workspaceID string
	payload     any
}

// fakeHub captures what the handlers publish, in order, so the payload shapes
// can be asserted without a WebSocket hub.
type fakeHub struct {
	events  []recordedEvent
	revoked [][2]string // {userID, channelID}; userID is "" for a revoke-for-all
}

func (f *fakeHub) record(method, workspaceID string, payload any) {
	f.events = append(f.events, recordedEvent{method: method, workspaceID: workspaceID, payload: payload})
}

func (f *fakeHub) PublishChannelCreated(workspaceID string, payload interface{}) {
	f.record("channel.created", workspaceID, payload)
}

func (f *fakeHub) PublishChannelUpdated(workspaceID string, payload interface{}) {
	f.record("channel.updated", workspaceID, payload)
}

func (f *fakeHub) PublishMemberJoined(workspaceID string, payload interface{}) {
	f.record("member.joined", workspaceID, payload)
}

func (f *fakeHub) PublishMemberLeft(workspaceID string, payload interface{}) {
	f.record("member.left", workspaceID, payload)
}

func (f *fakeHub) PublishUnreadUpdate(workspaceID string, payload interface{}) {
	f.record("unread.update", workspaceID, payload)
}

func (f *fakeHub) RevokeChannelSubscription(userID, channelID string) {
	f.revoked = append(f.revoked, [2]string{userID, channelID})
}

func (f *fakeHub) RevokeChannelForAll(channelID string) {
	f.revoked = append(f.revoked, [2]string{"", channelID})
}

// encode marshals a payload the way the hub does before it reaches the relay,
// so a key that only exists on the Go side (an unexported field, a missing
// json tag) fails the test.
func encode(t *testing.T, payload any) []byte {
	t.Helper()
	b, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

func decodeInto(t *testing.T, payload any, target any) {
	t.Helper()
	if err := json.Unmarshal(encode(t, payload), target); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
}

func only(t *testing.T, f *fakeHub, method string) recordedEvent {
	t.Helper()
	if len(f.events) != 1 {
		t.Fatalf("got %d events, want exactly 1: %+v", len(f.events), f.events)
	}
	if f.events[0].method != method {
		t.Fatalf("event: got %q, want %q", f.events[0].method, method)
	}
	return f.events[0]
}

func testChannel(chType ChannelType) *Channel {
	name := "release"
	slug := "release"
	return &Channel{
		ID:          "11111111-1111-1111-1111-111111111111",
		WorkspaceID: "22222222-2222-2222-2222-222222222222",
		Name:        &name,
		Slug:        &slug,
		Type:        chType,
	}
}

// channel.created is the one event whose audience depends on the payload: a
// public channel goes to the whole workspace, anything else only to member_ids.
// A missing "type" would announce a private channel to everyone; a missing
// "member_ids" would deliver it to nobody.
func TestChannelCreatedPayloadRoutesByTypeAndMembers(t *testing.T) {
	tests := []struct {
		name      string
		chType    ChannelType
		memberIDs []string
		wantMems  []string
	}{
		{name: "public", chType: TypePublic, memberIDs: []string{"u1"}, wantMems: []string{"u1"}},
		{name: "private", chType: TypePrivate, memberIDs: []string{"u1", "u2"}, wantMems: []string{"u1", "u2"}},
		{name: "dm", chType: TypeDM, memberIDs: []string{"u1", "u2"}, wantMems: []string{"u1", "u2"}},
		{name: "nil members encode as an empty list, never null", chType: TypePrivate, memberIDs: nil, wantMems: []string{}},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHub{}
			h := &Handler{hub: hub}
			ch := testChannel(tc.chType)

			h.emitChannelCreated(ch, tc.memberIDs)

			ev := only(t, hub, "channel.created")
			if ev.workspaceID != ch.WorkspaceID {
				t.Errorf("subject workspace: got %q, want %q", ev.workspaceID, ch.WorkspaceID)
			}

			var target relayCreatedTarget
			decodeInto(t, ev.payload, &target)
			if target.WorkspaceID != ch.WorkspaceID {
				t.Errorf("workspace_id: got %q, want %q", target.WorkspaceID, ch.WorkspaceID)
			}
			if target.Type != string(tc.chType) {
				t.Errorf("type: got %q, want %q", target.Type, tc.chType)
			}
			if target.MemberIDs == nil {
				t.Fatal("member_ids is missing; a non-public channel.created reaches nobody without it")
			}
			if len(target.MemberIDs) != len(tc.wantMems) {
				t.Fatalf("member_ids: got %v, want %v", target.MemberIDs, tc.wantMems)
			}
			for i, want := range tc.wantMems {
				if target.MemberIDs[i] != want {
					t.Errorf("member_ids[%d]: got %q, want %q", i, target.MemberIDs[i], want)
				}
			}
		})
	}
}

// channel.updated, member.joined and member.left are all delivered to the
// channel's subscribers, so each must carry a top-level "channel_id".
func TestChannelScopedPayloadsCarryChannelID(t *testing.T) {
	const (
		wsID   = "22222222-2222-2222-2222-222222222222"
		chID   = "11111111-1111-1111-1111-111111111111"
		userID = "33333333-3333-3333-3333-333333333333"
	)

	tests := []struct {
		name   string
		method string
		emit   func(h *Handler)
	}{
		{
			name:   "channel.updated",
			method: "channel.updated",
			emit:   func(h *Handler) { h.emitChannelUpdated(testChannel(TypePublic)) },
		},
		{
			name:   "member.joined",
			method: "member.joined",
			emit:   func(h *Handler) { h.emitMemberJoined(wsID, chID, userID, RoleMember) },
		},
		{
			name:   "member.left",
			method: "member.left",
			emit:   func(h *Handler) { h.emitMemberLeft(wsID, chID, userID) },
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			hub := &fakeHub{}
			h := &Handler{hub: hub}

			tc.emit(h)

			ev := only(t, hub, tc.method)
			if ev.workspaceID != wsID {
				t.Errorf("subject workspace: got %q, want %q", ev.workspaceID, wsID)
			}
			var target relayChannelTarget
			decodeInto(t, ev.payload, &target)
			if target.ChannelID != chID {
				t.Errorf("channel_id: got %q, want %q", target.ChannelID, chID)
			}
		})
	}
}

// unread.update is addressed to one person, so the relay routes it on
// "user_id" — a payload carrying only channel_id is dropped on the floor.
func TestUnreadUpdatePayloadCarriesUserID(t *testing.T) {
	const (
		wsID   = "22222222-2222-2222-2222-222222222222"
		chID   = "11111111-1111-1111-1111-111111111111"
		userID = "33333333-3333-3333-3333-333333333333"
	)

	hub := &fakeHub{}
	h := &Handler{hub: hub}

	h.emitUnreadUpdate(wsID, userID, &Unread{ChannelID: chID, Count: 3, LastReadAt: time.Unix(0, 0).UTC()})

	ev := only(t, hub, "unread.update")
	var target relayUserTarget
	decodeInto(t, ev.payload, &target)
	if target.UserID != userID {
		t.Errorf("user_id: got %q, want %q", target.UserID, userID)
	}

	var full struct {
		ChannelID string `json:"channel_id"`
		Count     int    `json:"unread_count"`
	}
	decodeInto(t, ev.payload, &full)
	if full.ChannelID != chID {
		t.Errorf("channel_id: got %q, want %q", full.ChannelID, chID)
	}
	if full.Count != 3 {
		t.Errorf("unread_count: got %d, want 3", full.Count)
	}
}

// A departure that publishes member.left but leaves the subscription in place
// keeps delivering the channel to the person who just lost access: delivery is
// gated on a map written only at subscribe time.
func TestMemberLeftRevokesTheSubscription(t *testing.T) {
	const (
		wsID   = "22222222-2222-2222-2222-222222222222"
		chID   = "11111111-1111-1111-1111-111111111111"
		userID = "33333333-3333-3333-3333-333333333333"
	)

	hub := &fakeHub{}
	h := &Handler{hub: hub}

	h.emitMemberLeft(wsID, chID, userID)

	if len(hub.revoked) != 1 {
		t.Fatalf("got %d revocations, want 1", len(hub.revoked))
	}
	if hub.revoked[0] != [2]string{userID, chID} {
		t.Errorf("revocation: got %v, want {%q %q}", hub.revoked[0], userID, chID)
	}
}

func TestRevokeChannelForAll(t *testing.T) {
	hub := &fakeHub{}
	h := &Handler{hub: hub}

	h.revokeChannelForAll("11111111-1111-1111-1111-111111111111")

	if len(hub.revoked) != 1 {
		t.Fatalf("got %d revocations, want 1", len(hub.revoked))
	}
	if hub.revoked[0] != [2]string{"", "11111111-1111-1111-1111-111111111111"} {
		t.Errorf("revocation: got %v, want a revoke-for-all", hub.revoked[0])
	}
}

// channel.member_added is consumed by notification.Service, which decodes it
// into ChannelMemberEvent. A renamed key is a notification that never fires.
func TestMemberAddedEventKeys(t *testing.T) {
	raw := encode(t, memberAddedEvent{
		ChannelID:   "11111111-1111-1111-1111-111111111111",
		ChannelName: "release",
		UserID:      "33333333-3333-3333-3333-333333333333",
		ActorID:     "44444444-4444-4444-4444-444444444444",
	})

	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	want := map[string]any{
		"channel_id":   "11111111-1111-1111-1111-111111111111",
		"channel_name": "release",
		"user_id":      "33333333-3333-3333-3333-333333333333",
		"actor_id":     "44444444-4444-4444-4444-444444444444",
	}
	if len(got) != len(want) {
		t.Fatalf("keys: got %v, want exactly %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s: got %v, want %v", k, got[k], v)
		}
	}
}

// The dedupe key must distinguish two different adds, or JetStream's duplicate
// window collapses them into one notification.
func TestEventKeyDistinguishesAdds(t *testing.T) {
	a := eventKey(evtChannelMemberAdded, "c1", "u1", "actor")
	b := eventKey(evtChannelMemberAdded, "c1", "u2", "actor")
	if a == b {
		t.Fatalf("two different adds share the dedupe key %q", a)
	}
	if want := "channel.member_added:c1:u1:actor"; a != want {
		t.Errorf("dedupe key: got %q, want %q", a, want)
	}
}

// The unit tests and any tooling build handlers without a hub. Publishing is
// best effort and must degrade to "no event", never to a panic on a request
// whose write already committed.
func TestEmittersTolerateANilHub(t *testing.T) {
	h := &Handler{}

	h.emitChannelCreated(testChannel(TypePrivate), []string{"u1"})
	h.emitChannelUpdated(testChannel(TypePublic))
	h.emitMemberJoined("ws", "ch", "u1", RoleMember)
	h.emitMemberLeft("ws", "ch", "u1")
	h.emitUnreadUpdate("ws", "u1", &Unread{ChannelID: "ch"})
	h.revokeChannelForAll("ch")
}

// A workspace id is spliced into the NATS subject, and a payload with no
// audience is a wasted broadcast: neither is publishable.
func TestEmittersSkipIncompletePayloads(t *testing.T) {
	hub := &fakeHub{}
	h := &Handler{hub: hub}

	h.emitChannelCreated(nil, []string{"u1"})
	h.emitChannelCreated(&Channel{ID: "ch"}, []string{"u1"}) // no workspace id
	h.emitChannelUpdated(nil)
	h.emitMemberJoined("", "ch", "u1", RoleMember)
	h.emitUnreadUpdate("ws", "u1", nil)

	if len(hub.events) != 0 {
		t.Fatalf("published %d events, want none: %+v", len(hub.events), hub.events)
	}
}
