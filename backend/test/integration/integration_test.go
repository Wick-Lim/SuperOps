//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
)

func TestHealthAndMetrics(t *testing.T) {
	h := getHarness(t)
	if code, _ := h.do(t, "GET", "/health", "", nil); code != 200 {
		t.Errorf("/health = %d, want 200", code)
	}
	// /metrics is plain text, not the JSON envelope.
	res, err := http.Get(h.base + "/metrics")
	if err != nil {
		t.Fatalf("/metrics: %v", err)
	}
	res.Body.Close()
	if res.StatusCode != 200 {
		t.Errorf("/metrics = %d, want 200", res.StatusCode)
	}
}

func TestAuthAndMe(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)

	code, r := h.do(t, "GET", "/api/v1/users/me", token, nil)
	if code != 200 {
		t.Fatalf("/users/me = %d", code)
	}
	var u struct {
		Email    string `json:"email"`
		Username string `json:"username"`
	}
	json.Unmarshal(r.Data, &u)
	if u.Email != adminEmail {
		t.Errorf("me.email = %q, want %q", u.Email, adminEmail)
	}

	// Wrong password is rejected.
	if code, _ := h.do(t, "POST", "/api/v1/auth/login", "", map[string]string{"email": adminEmail, "password": "nope"}); code != 401 {
		t.Errorf("bad login = %d, want 401", code)
	}
	// Missing token is rejected.
	if code, _ := h.do(t, "GET", "/api/v1/users/me", "", nil); code != 401 {
		t.Errorf("no-auth /users/me = %d, want 401", code)
	}
}

func TestMessagingFlow(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("msg"))

	// Send a message.
	code, r := h.do(t, "POST", "/api/v1/channels/"+chID+"/messages", token, map[string]string{"content": "hello integration"})
	if code != 201 {
		t.Fatalf("send = %d (%v)", code, r.Error)
	}
	var msg struct {
		ID      string `json:"id"`
		Content string `json:"content"`
	}
	json.Unmarshal(r.Data, &msg)
	if msg.ID == "" || msg.Content != "hello integration" {
		t.Fatalf("send returned %+v", msg)
	}

	// List returns it (and a pagination meta envelope).
	code, r = h.do(t, "GET", "/api/v1/channels/"+chID+"/messages?limit=10", token, nil)
	if code != 200 {
		t.Fatalf("list = %d", code)
	}
	if r.Meta == nil {
		t.Error("message list missing pagination meta")
	}
	var msgs []struct {
		ID        string `json:"id"`
		Reactions []any  `json:"reactions"`
	}
	json.Unmarshal(r.Data, &msgs)
	if len(msgs) == 0 {
		t.Fatal("list returned no messages")
	}

	// React, then confirm the reaction is hydrated on read.
	if code, _ := h.do(t, "POST", "/api/v1/channels/"+chID+"/messages/"+msg.ID+"/reactions", token, map[string]string{"emoji": "👍"}); code != 201 {
		t.Fatalf("react = %d", code)
	}
	code, r = h.do(t, "GET", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token, nil)
	if code != 200 {
		t.Fatalf("get message = %d", code)
	}
	var got struct {
		Reactions []struct {
			Emoji string `json:"emoji"`
		} `json:"reactions"`
	}
	json.Unmarshal(r.Data, &got)
	if len(got.Reactions) != 1 || got.Reactions[0].Emoji != "👍" {
		t.Errorf("reactions not hydrated: %+v", got.Reactions)
	}

	// Edit then delete.
	if code, _ := h.do(t, "PATCH", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token, map[string]string{"content": "edited"}); code != 200 {
		t.Errorf("edit = %d", code)
	}
	if code, _ := h.do(t, "DELETE", "/api/v1/channels/"+chID+"/messages/"+msg.ID, token, nil); code != 200 {
		t.Errorf("delete = %d", code)
	}
}

// TestRBACAndMembership exercises the invite flow, then verifies a plain member
// is denied admin endpoints and non-member channel access.
func TestRBACAndMembership(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)

	// Admin endpoint works for the admin.
	if code, _ := h.do(t, "GET", "/api/v1/admin/stats", admin, nil); code != 200 {
		t.Fatalf("admin stats (as admin) = %d, want 200", code)
	}

	// Create an invite and accept it as a brand-new member.
	email := fmt.Sprintf("member-%d@demo.local", time.Now().UnixNano())
	code, r := h.do(t, "POST", "/api/v1/admin/invitations", admin, map[string]string{"email": email, "role": "member"})
	if code != 201 {
		t.Fatalf("create invite = %d (%v)", code, r.Error)
	}
	var inv struct {
		Token string `json:"token"`
	}
	json.Unmarshal(r.Data, &inv)
	uname := fmt.Sprintf("member%d", time.Now().UnixNano())
	code, r = h.do(t, "POST", "/api/v1/auth/accept-invite", "", map[string]string{
		"token": inv.Token, "username": uname, "password": "memberpass123", "full_name": "Member",
	})
	if code != 201 {
		t.Fatalf("accept invite = %d (%v)", code, r.Error)
	}
	var tok struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(r.Data, &tok)
	member := tok.AccessToken
	if member == "" {
		t.Fatal("accept-invite returned no token")
	}

	// Member is denied the admin surface (RequireSystemAdmin).
	if code, _ := h.do(t, "GET", "/api/v1/admin/stats", member, nil); code != 403 {
		t.Errorf("admin stats (as member) = %d, want 403", code)
	}

	// Member is not in a channel the admin creates → membership-gated reads 403.
	chID := h.createChannel(t, admin, wsID, uniqueSlug("priv"))
	if code, _ := h.do(t, "GET", "/api/v1/workspaces/"+wsID+"/channels/"+chID+"/members", member, nil); code != 403 {
		t.Errorf("non-member ListMembers = %d, want 403", code)
	}
	if code, _ := h.do(t, "POST", "/api/v1/channels/"+chID+"/messages", member, map[string]string{"content": "x"}); code != 403 {
		t.Errorf("non-member send = %d, want 403", code)
	}
}

// TestRealtimeWebSocket is the end-to-end real-time test: it would have caught
// the http.Hijacker upgrade regression. It connects a WS client, subscribes to
// a channel, posts a message over REST, and asserts the client receives it.
func TestRealtimeWebSocket(t *testing.T) {
	h := getHarness(t)
	token := h.adminToken(t)
	wsID := h.firstWorkspace(t, token)
	chID := h.createChannel(t, token, wsID, uniqueSlug("rt"))

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	conn, _, err := websocket.Dial(ctx, h.wsURL(token), nil)
	if err != nil {
		t.Fatalf("WS dial failed (upgrade broken?): %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")

	// First frame should be hello.
	if typ := readType(t, ctx, conn); typ != "hello" {
		t.Fatalf("first frame = %q, want hello", typ)
	}

	// Subscribe to the channel (we are a member as its creator).
	writeJSON(t, ctx, conn, map[string]any{"type": "subscribe", "data": map[string]string{"channel_id": chID}})

	// Post a message via REST; the relay should deliver message.new over WS.
	want := "realtime-" + uniqueSlug("body")
	go func() {
		time.Sleep(300 * time.Millisecond)
		h.do(t, "POST", "/api/v1/channels/"+chID+"/messages", token, map[string]string{"content": want})
	}()

	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		typ, data := readFrame(t, ctx, conn)
		if typ == "message.new" {
			var m struct {
				Content   string `json:"content"`
				ChannelID string `json:"channel_id"`
			}
			json.Unmarshal(data, &m)
			if m.Content == want && m.ChannelID == chID {
				return // success
			}
		}
	}
	t.Fatal("did not receive message.new over WebSocket within deadline")
}

// TestWebSocketSubscribeAuthz confirms a non-member cannot subscribe.
func TestWebSocketSubscribeAuthz(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	wsID := h.firstWorkspace(t, admin)

	// New member who is NOT in the channel.
	email := fmt.Sprintf("wsmember-%d@demo.local", time.Now().UnixNano())
	_, r := h.do(t, "POST", "/api/v1/admin/invitations", admin, map[string]string{"email": email, "role": "member"})
	var inv struct{ Token string `json:"token"` }
	json.Unmarshal(r.Data, &inv)
	uname := fmt.Sprintf("wsmember%d", time.Now().UnixNano())
	_, r = h.do(t, "POST", "/api/v1/auth/accept-invite", "", map[string]string{"token": inv.Token, "username": uname, "password": "memberpass123", "full_name": "WS"})
	var tok struct{ AccessToken string `json:"access_token"` }
	json.Unmarshal(r.Data, &tok)
	chID := h.createChannel(t, admin, wsID, uniqueSlug("wsauthz"))

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, h.wsURL(tok.AccessToken), nil)
	if err != nil {
		t.Fatalf("WS dial: %v", err)
	}
	defer conn.Close(websocket.StatusNormalClosure, "")
	readType(t, ctx, conn) // hello
	writeJSON(t, ctx, conn, map[string]any{"type": "subscribe", "data": map[string]string{"channel_id": chID}})
	if typ := readType(t, ctx, conn); typ != "error" {
		t.Errorf("non-member subscribe response = %q, want error", typ)
	}
}

// --- WS frame helpers ---

func readFrame(t *testing.T, ctx context.Context, c *websocket.Conn) (string, json.RawMessage) {
	t.Helper()
	_, data, err := c.Read(ctx)
	if err != nil {
		t.Fatalf("ws read: %v", err)
	}
	var f struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if err := json.Unmarshal(data, &f); err != nil {
		t.Fatalf("ws frame unmarshal: %v (%s)", err, string(data))
	}
	return f.Type, f.Data
}

func readType(t *testing.T, ctx context.Context, c *websocket.Conn) string {
	typ, _ := readFrame(t, ctx, c)
	return typ
}

func writeJSON(t *testing.T, ctx context.Context, c *websocket.Conn, v any) {
	t.Helper()
	b, _ := json.Marshal(v)
	if err := c.Write(ctx, websocket.MessageText, b); err != nil {
		t.Fatalf("ws write: %v", err)
	}
}
