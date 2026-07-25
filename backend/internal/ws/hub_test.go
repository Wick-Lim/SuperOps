package ws

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"
)

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestClient builds a Client with no underlying socket. Nothing the hub does
// touches the connection; only the pumps and close() do.
func newTestClient(hub *Hub, userID string, workspaceIDs ...string) *Client {
	return NewClient(context.Background(), hub, nil, userID, workspaceIDs, nil, nil, nil, testLogger())
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// TestHubRegisterEvictConcurrent hammers the registry from many goroutines
// while broadcasting and reading counters. Run with -race: the previous
// implementation read len(h.clients) after releasing the lock.
func TestHubRegisterEvictConcurrent(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()
	defer hub.Shutdown()

	const users, connsPerUser = 8, 8

	var wg sync.WaitGroup
	for u := 0; u < users; u++ {
		userID := fmt.Sprintf("user-%d", u)
		for i := 0; i < connsPerUser; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				c := newTestClient(hub, userID, "ws-1")
				hub.register <- c
				c.Subscribe("chan-1")

				hub.BroadcastLocal("chan-1", TypeMessageNew, map[string]string{"channel_id": "chan-1"}, "")
				hub.BroadcastToUser(userID, TypeNotificationNew, map[string]string{"user_id": userID})
				hub.BroadcastToWorkspaceLocal("ws-1", TypePresenceChanged, map[string]string{"workspace_id": "ws-1"}, "")
				_ = hub.GetOnlineUserIDs()
				_ = hub.ConnectionCount()
				_ = hub.IsOnline(userID)

				hub.unregister <- c
			}()
		}
	}
	wg.Wait()

	waitFor(t, "all connections to drain", func() bool { return hub.ConnectionCount() == 0 })
	if got := len(hub.GetOnlineUserIDs()); got != 0 {
		t.Fatalf("online users after drain = %d, want 0", got)
	}
}

// TestHubMultipleConnectionsPerUser is the phone-and-laptop case: a second
// connection must not evict the first, and both must receive user-targeted
// frames.
func TestHubMultipleConnectionsPerUser(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()
	defer hub.Shutdown()

	phone := newTestClient(hub, "u1")
	laptop := newTestClient(hub, "u1")

	hub.register <- phone
	hub.register <- laptop
	waitFor(t, "both connections registered", func() bool { return hub.ConnectionCount() == 2 })

	hub.BroadcastToUser("u1", TypeNotificationNew, map[string]string{"user_id": "u1"})
	waitFor(t, "fan-out to both connections", func() bool {
		return len(phone.send) == 1 && len(laptop.send) == 1
	})

	hub.unregister <- phone
	waitFor(t, "one connection left", func() bool { return hub.ConnectionCount() == 1 })
	if !hub.IsOnline("u1") {
		t.Fatal("user reported offline while a second connection is live")
	}

	hub.unregister <- laptop
	waitFor(t, "user offline", func() bool { return !hub.IsOnline("u1") })
}

// TestSeqMonotonicPerConnection asserts every frame on a connection carries the
// next sequence number, in enqueue order, even under concurrent senders.
func TestSeqMonotonicPerConnection(t *testing.T) {
	hub := NewHub(testLogger())
	c := newTestClient(hub, "u1")

	const frames = 200
	var wg sync.WaitGroup
	for i := 0; i < frames; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c.SendMessage(TypePong, nil)
		}()
	}
	wg.Wait()
	close(c.send)

	var prev int64
	for b := range c.send {
		var m OutboundMessage
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if m.Seq != prev+1 {
			t.Fatalf("seq = %d, want %d (frames must be strictly monotonic in stream order)", m.Seq, prev+1)
		}
		prev = m.Seq
	}
	if prev != frames {
		t.Fatalf("last seq = %d, want %d", prev, frames)
	}
}

// TestBroadcastStampsSeq covers the regression that channel-scoped frames all
// shipped seq 0 while user-targeted frames incremented.
func TestBroadcastStampsSeq(t *testing.T) {
	hub := NewHub(testLogger())
	c := newTestClient(hub, "u1")
	c.Subscribe("chan-1")

	hub.mu.Lock()
	hub.clients["u1"] = map[string]*Client{c.connID: c}
	hub.total = 1
	hub.mu.Unlock()

	hub.BroadcastLocal("chan-1", TypeMessageNew, map[string]string{"channel_id": "chan-1"}, "")
	hub.BroadcastToUser("u1", TypeNotificationNew, map[string]string{"user_id": "u1"})

	for want := int64(1); want <= 2; want++ {
		var m OutboundMessage
		if err := json.Unmarshal(<-c.send, &m); err != nil {
			t.Fatalf("unmarshal frame: %v", err)
		}
		if m.Seq != want {
			t.Fatalf("seq = %d, want %d", m.Seq, want)
		}
	}
}

func TestRevokeChannelSubscription(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()
	defer hub.Shutdown()

	c := newTestClient(hub, "u1")
	hub.register <- c
	waitFor(t, "registered", func() bool { return hub.ConnectionCount() == 1 })
	c.Subscribe("chan-1")

	hub.RevokeChannelSubscription("u1", "chan-1")

	if c.IsSubscribed("chan-1") {
		t.Fatal("subscription survived revocation")
	}
	var m OutboundMessage
	if err := json.Unmarshal(<-c.send, &m); err != nil {
		t.Fatalf("unmarshal frame: %v", err)
	}
	if m.Type != TypeUnsubscribed {
		t.Fatalf("frame type = %q, want %q", m.Type, TypeUnsubscribed)
	}

	// Delivery must stop immediately.
	hub.BroadcastLocal("chan-1", TypeMessageNew, map[string]string{"channel_id": "chan-1"}, "")
	if len(c.send) != 0 {
		t.Fatalf("revoked client still received %d frames", len(c.send))
	}
}

// TestBackpressureClosesSlowClient asserts a client that cannot drain its
// buffer is disconnected instead of being left silently desynced.
func TestBackpressureClosesSlowClient(t *testing.T) {
	hub := NewHub(testLogger())
	c := newTestClient(hub, "u1")

	for i := 0; i < sendBuffer+maxConsecutiveDrops; i++ {
		c.SendMessage(TypePong, nil)
	}

	waitFor(t, "slow client to be closed", func() bool { return c.ctx.Err() != nil })
}

// TestShutdownStopsRunAndReleasesPumps covers the previously dead Shutdown: it
// must stop Run and must not leave a read pump blocked on the unregister
// channel forever.
func TestShutdownStopsRunAndReleasesPumps(t *testing.T) {
	hub := NewHub(testLogger())
	go hub.Run()

	c := newTestClient(hub, "u1")
	hub.register <- c
	waitFor(t, "registered", func() bool { return hub.ConnectionCount() == 1 })

	hub.Shutdown()
	hub.Shutdown() // idempotent

	if hub.ConnectionCount() != 0 {
		t.Fatalf("connections after shutdown = %d, want 0", hub.ConnectionCount())
	}
	if c.ctx.Err() == nil {
		t.Fatal("client context still live after shutdown")
	}

	select {
	case hub.unregister <- c:
		t.Fatal("unregister accepted after shutdown; Run should have exited")
	case <-hub.done:
	}
}

// TestDispatchChannelCreatedPrivate asserts a private channel is announced only
// to its initial members, not to the whole workspace.
func TestDispatchChannelCreatedPrivate(t *testing.T) {
	hub := NewHub(testLogger())
	member := newTestClient(hub, "member", "ws-1")
	bystander := newTestClient(hub, "bystander", "ws-1")

	hub.mu.Lock()
	hub.clients["member"] = map[string]*Client{member.connID: member}
	hub.clients["bystander"] = map[string]*Client{bystander.connID: bystander}
	hub.total = 2
	hub.mu.Unlock()

	payload, err := json.Marshal(map[string]interface{}{
		"id":           "chan-1",
		"workspace_id": "ws-1",
		"type":         "private",
		"member_ids":   []string{"member"},
	})
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	hub.dispatchEvent(TypeChannelCreated, payload)

	if len(member.send) != 1 {
		t.Fatalf("member received %d frames, want 1", len(member.send))
	}
	if len(bystander.send) != 0 {
		t.Fatalf("private channel leaked to %d non-member frames", len(bystander.send))
	}

	// A public channel goes to the whole workspace.
	payload, _ = json.Marshal(map[string]interface{}{
		"id": "chan-2", "workspace_id": "ws-1", "type": "public",
	})
	hub.dispatchEvent(TypeChannelCreated, payload)
	if len(bystander.send) != 1 {
		t.Fatalf("public channel not announced to workspace member (got %d frames)", len(bystander.send))
	}
}
