//go:build integration

package integration

import (
	"encoding/json"
	"net/http"
	"testing"
	"time"
)

// wsDeliveryTimeout is generous on purpose: a message.new travels REST handler
// → JetStream publish + storage ack → core NATS fan-out → relay → hub → write
// pump before it reaches the socket.
const wsDeliveryTimeout = 15 * time.Second

// wsQuietPeriod is how long "nothing should arrive" assertions wait. It only
// has to outlast the delivery path above for a message posted before it starts.
const wsQuietPeriod = 3 * time.Second

func channelIDOf(t *testing.T, f wsFrame) string {
	t.Helper()
	var d struct {
		ChannelID string `json:"channel_id"`
	}
	if err := json.Unmarshal(f.data, &d); err != nil {
		t.Fatalf("%s frame payload: %v (%s)", f.typ, err, string(f.data))
	}
	return d.ChannelID
}

// TestRealtimeWebSocket is the end-to-end real-time test: it would have caught
// the http.Hijacker upgrade regression (the envelope wrapper swallowed the
// hijack, so every upgrade 500'd and nothing noticed). It connects a WS client,
// subscribes to a channel, posts a message over REST, and asserts the client
// receives it.
//
// It matches frames by type instead of asserting on the very next one. The
// protocol grew a subscribed ack, per-frame sequence numbers, unread.update and
// presence.changed pushes since this test was written, and every one of them
// made the "next frame is X" form flaky.
func TestRealtimeWebSocket(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("rt"))

	c := h.dialWS(t, token)

	hello := c.waitFor(10*time.Second, "hello")
	var greeting struct {
		UserID       string `json:"user_id"`
		ConnectionID string `json:"connection_id"`
	}
	decodeInto(t, hello.data, &greeting)
	if greeting.UserID == "" || greeting.ConnectionID == "" {
		t.Errorf("hello frame = %s, want user_id and connection_id", string(hello.data))
	}

	// The subscribe ack is what tells a client its subscription is live. Without
	// waiting for it the post below races the subscription and anything sent in
	// the gap is lost — which is exactly why the ack was added.
	c.subscribe(chID)
	ack := c.waitFor(10*time.Second, "subscribed", "error")
	if ack.typ != "subscribed" {
		t.Fatalf("subscribe answered %s: %s", ack.typ, string(ack.data))
	}
	if got := channelIDOf(t, ack); got != chID {
		t.Fatalf("subscribed ack for %q, want %q", got, chID)
	}

	// The client-driven ping still works alongside the server-driven one.
	c.send(map[string]any{"type": "ping"})
	c.waitFor(10*time.Second, "pong")

	want := "realtime-" + uniqueSlug("body")
	h.postMessage(t, token, chID, want)

	deadline := time.Now().Add(wsDeliveryTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("did not receive the posted message over the WebSocket")
		}
		f := c.waitFor(remaining, "message.new")
		var m struct {
			Content   string `json:"content"`
			ChannelID string `json:"channel_id"`
		}
		decodeInto(t, f.data, &m)
		if m.Content == want && m.ChannelID == chID {
			break
		}
	}

	// Unsubscribing is acknowledged too, and with the reason that distinguishes
	// it from a server-side revocation.
	c.send(map[string]any{"type": "unsubscribe", "data": map[string]string{"channel_id": chID}})
	off := c.waitFor(10*time.Second, "unsubscribed")
	var reason struct {
		ChannelID string `json:"channel_id"`
		Reason    string `json:"reason"`
	}
	decodeInto(t, off.data, &reason)
	if reason.ChannelID != chID || reason.Reason != "client" {
		t.Errorf("unsubscribed frame = %s, want channel %s reason client", string(off.data), chID)
	}

	// And delivery really stops.
	h.postMessage(t, token, chID, "after-unsubscribe-"+uniqueSlug("x"))
	c.expectNone(wsQuietPeriod, "message.new")
}

// TestWebSocketSubscribeAuthz confirms a non-member cannot subscribe, and that
// the refusal is an error frame carrying a machine-readable code rather than a
// bare string a client has to pattern-match.
func TestWebSocketSubscribeAuthz(t *testing.T) {
	h := getHarness(t)
	owner := h.newTenant(t, "wsauthz")
	chID := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("wsauthz"))

	stranger := h.newTenant(t, "wsintr")
	c := h.dialWS(t, stranger.token)
	c.waitFor(10*time.Second, "hello")

	c.subscribe(chID)
	f := c.waitFor(10*time.Second, "error", "subscribed")
	if f.typ != "subscribed" {
		var e struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		decodeInto(t, f.data, &e)
		if e.Code != "FORBIDDEN" {
			t.Errorf("non-member subscribe error code = %q, want FORBIDDEN (%s)", e.Code, e.Message)
		}
	} else {
		t.Fatalf("non-member subscribe was acknowledged: %s", string(f.data))
	}

	// The refusal must also mean no delivery, not just no ack.
	h.postMessage(t, owner.token, chID, "members only "+uniqueSlug("x"))
	c.expectNone(wsQuietPeriod, "message.new")
}

// TestWebSocketRevocation covers the other half of the subscription model:
// delivery is gated on an in-memory map written only at subscribe time, so
// removing somebody from a channel has to reach the hub or their socket keeps
// receiving that channel's traffic for as long as it stays open.
func TestWebSocketRevocation(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	seed := h.firstWorkspace(t, admin)

	owner := h.newTenant(t, "wsrevo")
	member := h.newUser(t, admin, seed, "wsrevm")
	h.joinWorkspace(t, owner.token, owner.workspaceID, member)

	chID := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("wsrev"))
	h.req(t, http.StatusOK, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+chID+"/join", member.token, nil)

	c := h.dialWS(t, member.token)
	c.waitFor(10*time.Second, "hello")
	c.subscribe(chID)
	if ack := c.waitFor(10*time.Second, "subscribed", "error"); ack.typ != "subscribed" {
		t.Fatalf("member subscribe answered %s: %s", ack.typ, string(ack.data))
	}

	// Baseline: delivery works while the membership stands.
	first := "before-revoke-" + uniqueSlug("x")
	h.postMessage(t, owner.token, chID, first)
	deadline := time.Now().Add(wsDeliveryTimeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatal("subscribed member never received the baseline message")
		}
		var m struct {
			Content string `json:"content"`
		}
		decodeInto(t, c.waitFor(remaining, "message.new").data, &m)
		if m.Content == first {
			break
		}
	}

	h.req(t, http.StatusOK, "DELETE",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+chID+"/members/"+member.id, owner.token, nil)

	revoked := c.waitFor(wsDeliveryTimeout, "unsubscribed")
	var reason struct {
		ChannelID string `json:"channel_id"`
		Reason    string `json:"reason"`
	}
	decodeInto(t, revoked.data, &reason)
	if reason.ChannelID != chID || reason.Reason != "revoked" {
		t.Errorf("revocation frame = %s, want channel %s reason revoked", string(revoked.data), chID)
	}

	// The socket must now be as deaf as the REST API is.
	h.postMessage(t, owner.token, chID, "after-revoke-"+uniqueSlug("x"))
	c.expectNone(wsQuietPeriod, "message.new")
	h.denied(t, http.StatusForbidden, "GET", "/api/v1/channels/"+chID+"/messages", member.token, nil)

	// Re-subscribing is refused as well, so the revocation is not merely a
	// one-shot frame the client can undo.
	c.subscribe(chID)
	if f := c.waitFor(10*time.Second, "error", "subscribed"); f.typ != "error" {
		t.Errorf("re-subscribe after removal answered %s: %s", f.typ, string(f.data))
	}
}
