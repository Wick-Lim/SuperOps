//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/channel"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// The unread badge is published by cmd/worker's unread-fanout durable, and this
// suite runs the API alone — exactly as the search tests index their own
// fixtures because nothing in the API process writes to Meilisearch. So the
// consumer callback is driven directly with the event the REST send path really
// published, and everything after it (the batched count query, the NATS
// publish, the relay's user_id routing, the hub, the socket) is the real thing.
func runUnreadFanout(t *testing.T, h *harness, workspaceID, eventType string, payload map[string]any) {
	t.Helper()
	fanout := channel.NewUnreadFanout(channel.NewRepository(h.app.DB), h.app.NATS, logger.New("error"))

	body, err := json.Marshal(natspkg.Event{Type: eventType, Data: payload})
	if err != nil {
		t.Fatalf("marshal %s envelope: %v", eventType, err)
	}
	subject := "superops." + workspaceID + "." + subjectFor(eventType)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := fanout.HandleMessage(ctx, &nats.Msg{Subject: subject, Data: body}); err != nil {
		t.Fatalf("unread fan-out for %s: %v", eventType, err)
	}
}

func subjectFor(eventType string) string {
	if eventType == "message.deleted" {
		return "message.deleted"
	}
	return "message.created"
}

type unreadFrame struct {
	UserID      string `json:"user_id"`
	ChannelID   string `json:"channel_id"`
	UnreadCount int    `json:"unread_count"`
}

// awaitUnread waits for an unread.update naming channelID, ignoring badges for
// other channels the connection may be told about.
func awaitUnread(t *testing.T, c *wsClient, channelID string, timeout time.Duration) unreadFrame {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			t.Fatalf("no unread.update for channel %s within %s", channelID, timeout)
		}
		f, ok := c.poll(remaining, "unread.update")
		if !ok {
			t.Fatalf("no unread.update for channel %s within %s", channelID, timeout)
		}
		var u unreadFrame
		decodeInto(t, f.data, &u)
		if u.ChannelID == channelID {
			return u
		}
	}
}

// TestUnreadBadgeRisesForOtherMembers is the regression test for the half of the
// unread feature that was missing: unread.update had exactly one publisher, the
// mark-read handler, so the badge cleared but never lit up.
//
// It asserts the badge rises for a second member when a message is posted, that
// the pushed count agrees with the count GET /channels/{id}/unread computes
// (the two must never disagree — see Repository.UnreadCounts), that the author
// is excluded, and that deleting the message moves the badge back down.
func TestUnreadBadgeRisesForOtherMembers(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)
	adminID := h.userID(t, admin)
	chID := h.createChannel(t, admin, wsID, uniqueSlug("unread"))

	bob := h.newUser(t, admin, wsID, "unreadbob")
	h.req(t, http.StatusCreated, "POST",
		"/api/v1/workspaces/"+wsID+"/channels/"+chID+"/members", admin,
		map[string]string{"user_id": bob.id})

	// Bob's badge starts at zero: he was added after the channel was created and
	// nothing has been posted.
	if got := unreadOf(t, h, bob.token, chID); got != 0 {
		t.Fatalf("bob's initial unread = %d, want 0", got)
	}

	bobWS := h.dialWS(t, bob.token)
	bobWS.waitFor(10*time.Second, "hello")
	authorWS := h.dialWS(t, admin)
	authorWS.waitFor(10*time.Second, "hello")

	// Deliberately NOT subscribed to the channel: unread.update is addressed to
	// a user, not to a channel's subscribers. A client whose sidebar shows the
	// badge has not opened the channel.
	msg := h.postMessage(t, admin, chID, "unread badge should light up")

	runUnreadFanout(t, h, wsID, "message.new", map[string]any{
		"id":         msg.ID,
		"channel_id": chID,
		"user_id":    adminID,
	})

	got := awaitUnread(t, bobWS, chID, wsDeliveryTimeout)
	if got.UserID != bob.id {
		t.Errorf("unread.update user_id = %q, want bob (%q)", got.UserID, bob.id)
	}
	if got.UnreadCount != 1 {
		t.Errorf("unread.update unread_count = %d, want 1", got.UnreadCount)
	}
	// The push and the authoritative REST count must agree.
	if rest := unreadOf(t, h, bob.token, chID); rest != got.UnreadCount {
		t.Errorf("pushed unread_count %d disagrees with GET /unread = %d", got.UnreadCount, rest)
	}

	// The author's own badge did not move, so the author gets no frame.
	authorWS.expectNone(wsQuietPeriod, "unread.update")

	// Deleting the message moves the badge back down for everyone.
	h.req(t, http.StatusOK, "DELETE",
		"/api/v1/channels/"+chID+"/messages/"+msg.ID, admin, nil)
	runUnreadFanout(t, h, wsID, "message.deleted", map[string]any{
		"id":         msg.ID,
		"channel_id": chID,
		"parent_id":  nil,
	})

	cleared := awaitUnread(t, bobWS, chID, wsDeliveryTimeout)
	if cleared.UnreadCount != 0 {
		t.Errorf("after delete, unread.update unread_count = %d, want 0", cleared.UnreadCount)
	}
}

// TestUnreadFanoutIgnoresThreadReplies pins the other half of the contract: no
// unread query counts thread replies, so a reply must not produce a badge push
// at all. Without the skip every reply would publish an unchanged count to
// every member of the channel.
func TestUnreadFanoutIgnoresThreadReplies(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)
	adminID := h.userID(t, admin)
	chID := h.createChannel(t, admin, wsID, uniqueSlug("unreadthread"))

	bob := h.newUser(t, admin, wsID, "unreadthr")
	h.req(t, http.StatusCreated, "POST",
		"/api/v1/workspaces/"+wsID+"/channels/"+chID+"/members", admin,
		map[string]string{"user_id": bob.id})

	root := h.postMessage(t, admin, chID, "thread root")

	bobWS := h.dialWS(t, bob.token)
	bobWS.waitFor(10*time.Second, "hello")

	r := h.req(t, http.StatusCreated, "POST", "/api/v1/messages/"+root.ID+"/thread", admin,
		map[string]string{"content": "a reply"})
	var reply postedMessage
	decodeInto(t, r.Data, &reply)

	runUnreadFanout(t, h, wsID, "message.new", map[string]any{
		"id":         reply.ID,
		"channel_id": chID,
		"user_id":    adminID,
		"parent_id":  root.ID,
	})

	bobWS.expectNone(wsQuietPeriod, "unread.update")

	// And the badge really is one — the root only.
	if got := unreadOf(t, h, bob.token, chID); got != 1 {
		t.Errorf("bob's unread = %d, want 1 (the root, not the reply)", got)
	}
}

func unreadOf(t *testing.T, h *harness, token, channelID string) int {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+channelID+"/unread", token, nil)
	var u struct {
		UnreadCount int `json:"unread_count"`
	}
	decodeInto(t, r.Data, &u)
	return u.UnreadCount
}
