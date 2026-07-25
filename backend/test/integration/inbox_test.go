//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/pkg/logger"
	natspkg "github.com/Wick-Lim/SuperOps/backend/pkg/nats"
)

// The inbox fan-out runs in cmd/worker's notifier-message durable, and this
// suite runs the API alone — so the consumer callback is driven directly with
// the event the REST send path really published, exactly as unread_test.go does
// for the unread badge. Everything after the callback (the two-table
// transaction, the coalescing, the item projection, the routes) is the real
// thing running against the wired app's pool.
func runNotifier(t *testing.T, h *harness, workspaceID string, m postedMessage) {
	t.Helper()
	runNotifierN(t, h, workspaceID, m, 1)
}

// runNotifierN replays one message.created event n times, which is what a lost
// ack or a nak looks like from the handler's side.
func runNotifierN(t *testing.T, h *harness, workspaceID string, m postedMessage, n int) {
	t.Helper()
	notifier := inbox.NewNotifier(inbox.NewRepository(h.app.DB), h.app.NATS, nil, nil, logger.New("error"))
	svc := notification.NewService(h.app.DB, authz.New(h.app.DB), notifier, logger.New("error"))

	body, err := json.Marshal(natspkg.Event{Type: "message.new", Data: map[string]any{
		"id": m.ID, "channel_id": m.ChannelID, "user_id": m.UserID, "content": m.Content,
	}})
	if err != nil {
		t.Fatalf("marshal message.new envelope: %v", err)
	}
	subject := "superops." + workspaceID + ".message.created"

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for range n {
		if err := svc.HandleMessage(ctx, &nats.Msg{Subject: subject, Data: body}); err != nil {
			t.Fatalf("notifier fan-out: %v", err)
		}
	}
}

type inboxItem struct {
	ID          string `json:"id"`
	WorkspaceID string `json:"workspace_id"`
	SubjectType string `json:"subject_type"`
	SubjectID   string `json:"subject_id"`
	State       string `json:"state"`
	TopKind     string `json:"top_kind"`
	UnreadCount int    `json:"unread_count"`
	EventCount  int    `json:"event_count"`
	Title       string `json:"title"`
}

func (h *harness) inbox(t *testing.T, token, query string) []inboxItem {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/inbox"+query, token, nil)
	var items []inboxItem
	decodeInto(t, r.Data, &items)
	return items
}

func (h *harness) inboxCount(t *testing.T, token string) int {
	t.Helper()
	r := h.req(t, http.StatusOK, "GET", "/api/v1/inbox/count", token, nil)
	var out struct {
		Unread int `json:"unread"`
	}
	decodeInto(t, r.Data, &out)
	return out.Unread
}

// mentionFixture is a public channel with two members, one of whom gets
// mentioned.
type mentionFixture struct {
	owner     *tenant
	recipient *actor
	channelID string
	username  string
}

func newMentionFixture(t *testing.T, h *harness) *mentionFixture {
	t.Helper()
	owner := h.newTenant(t, "inbox")
	recipient := h.newUser(t, owner.token, owner.workspaceID, "inboxrcpt")
	channelID := h.createChannel(t, owner.token, owner.workspaceID, uniqueSlug("inbox"))

	h.req(t, http.StatusCreated, "POST",
		"/api/v1/workspaces/"+owner.workspaceID+"/channels/"+channelID+"/members", owner.token,
		map[string]string{"user_id": recipient.id})

	r := h.req(t, http.StatusOK, "GET", "/api/v1/users/me", recipient.token, nil)
	var me struct {
		Username string `json:"username"`
	}
	decodeInto(t, r.Data, &me)
	if me.Username == "" {
		t.Fatal("recipient has no username")
	}
	return &mentionFixture{owner: owner, recipient: recipient, channelID: channelID, username: me.Username}
}

func (f *mentionFixture) mention(t *testing.T, h *harness, n int) {
	t.Helper()
	for i := range n {
		m := h.postMessage(t, f.owner.token, f.channelID,
			fmt.Sprintf("@%s look at this %d", f.username, i))
		runNotifier(t, h, f.owner.workspaceID, m)
	}
}

// Forty mentions in one channel is ONE thing that needs attention, not forty.
// That is what makes the badge comparable to the list: a bell showing 40 over a
// single row is the bug, not the fix.
func TestInboxCoalescesBurst(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	const burst = 40
	f.mention(t, h, burst)

	items := h.inbox(t, f.recipient.token, "")
	if len(items) != 1 {
		t.Fatalf("%d inbox items after %d mentions, want 1: %+v", len(items), burst, items)
	}
	it := items[0]
	if it.UnreadCount != burst || it.EventCount != burst {
		t.Fatalf("unread_count/event_count = %d/%d, want %d/%d", it.UnreadCount, it.EventCount, burst, burst)
	}
	if it.SubjectID != f.channelID || it.SubjectType != "channel" {
		t.Fatalf("item subject = %s/%s, want channel/%s", it.SubjectType, it.SubjectID, f.channelID)
	}
	if it.TopKind != inbox.KindMessageMention {
		t.Fatalf("top_kind = %q, want %q", it.TopKind, inbox.KindMessageMention)
	}
	if n := h.inboxCount(t, f.recipient.token); n != 1 {
		t.Fatalf("GET /inbox/count = %d after a burst of %d, want 1", n, burst)
	}

	// The events behind the item are all there and pageable.
	r := h.req(t, http.StatusOK, "GET", "/api/v1/inbox/"+it.ID+"/events?limit=100", f.recipient.token, nil)
	var events []struct {
		Kind string `json:"kind"`
	}
	decodeInto(t, r.Data, &events)
	if len(events) != burst {
		t.Fatalf("%d events behind the item, want %d", len(events), burst)
	}
}

// THE test that guards the hard part. Redeliver the same event several times and
// assert the count moves EXACTLY ONCE.
//
// With a coalesced counter an ungated redelivery does not produce an invisible
// duplicate row — it produces a badge that is permanently wrong by one, with
// nothing to correct it from. The gate is the ON CONFLICT DO NOTHING on
// inbox_events, in the same transaction as the item upsert.
func TestInboxRedeliveryIsIdempotent(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	m := h.postMessage(t, f.owner.token, f.channelID, "@"+f.username+" once")
	runNotifierN(t, h, f.owner.workspaceID, m, 5)

	items := h.inbox(t, f.recipient.token, "")
	if len(items) != 1 {
		t.Fatalf("%d items after 5 redeliveries of one event, want 1", len(items))
	}
	if items[0].UnreadCount != 1 || items[0].EventCount != 1 {
		t.Fatalf("unread_count/event_count = %d/%d after 5 redeliveries, want 1/1",
			items[0].UnreadCount, items[0].EventCount)
	}
	if n := h.inboxCount(t, f.recipient.token); n != 1 {
		t.Fatalf("badge = %d after 5 redeliveries, want 1", n)
	}
}

// The one overlap between the inbox and the channel unread badge. They cover
// disjoint things everywhere else — a plain channel message never creates an
// inbox item — and they have to AGREE here, in the same transaction, or the user
// reads #alerts, sees the channel badge clear, and watches the bell keep saying
// 1.
func TestInboxAgreesWithChannelUnread(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	f.mention(t, h, 3)
	if n := h.inboxCount(t, f.recipient.token); n != 1 {
		t.Fatalf("badge = %d before reading, want 1", n)
	}

	h.req(t, http.StatusOK, "PUT", "/api/v1/channels/"+f.channelID+"/read", f.recipient.token, nil)

	if n := h.inboxCount(t, f.recipient.token); n != 0 {
		t.Fatalf("inbox badge = %d after marking the channel read, want 0", n)
	}
	r := h.req(t, http.StatusOK, "GET", "/api/v1/channels/"+f.channelID+"/unread", f.recipient.token, nil)
	var unread struct {
		UnreadCount int `json:"unread_count"`
	}
	decodeInto(t, r.Data, &unread)
	if unread.UnreadCount != 0 {
		t.Fatalf("channel unread = %d after marking read, want 0", unread.UnreadCount)
	}

	// The item is still LISTED — read, not gone. Only 'done' removes it from the
	// default view, and only the user does that.
	items := h.inbox(t, f.recipient.token, "")
	if len(items) != 1 || items[0].State != inbox.StateRead {
		t.Fatalf("after reading the channel the item is %+v, want one item in state 'read'", items)
	}
}

// A plain channel message must NOT create an inbox item. That is the rule that
// keeps the two badge systems disjoint, and it is also what keeps a busy channel
// from producing one row update per member per message.
func TestPlainChannelMessageCreatesNoInboxItem(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	m := h.postMessage(t, f.owner.token, f.channelID, "no mention here")
	runNotifier(t, h, f.owner.workspaceID, m)

	if items := h.inbox(t, f.recipient.token, ""); len(items) != 0 {
		t.Fatalf("an undirected channel message produced %d inbox items: %+v", len(items), items)
	}
	if n := h.inboxCount(t, f.recipient.token); n != 0 {
		t.Fatalf("badge = %d after an undirected message, want 0", n)
	}
}

// 'done' is archive, not mute: a conversation that comes back needs to come back.
func TestInboxDoneReopensOnNewEvent(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	f.mention(t, h, 1)
	items := h.inbox(t, f.recipient.token, "")
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}
	id := items[0].ID

	h.req(t, http.StatusOK, "PUT", "/api/v1/inbox/"+id+"/done", f.recipient.token, nil)
	if got := h.inbox(t, f.recipient.token, ""); len(got) != 0 {
		t.Fatalf("a done item is still in the default (open) view: %+v", got)
	}
	if got := h.inbox(t, f.recipient.token, "?state=done"); len(got) != 1 {
		t.Fatalf("%d items with state=done, want 1", len(got))
	}

	f.mention(t, h, 1)

	got := h.inbox(t, f.recipient.token, "")
	if len(got) != 1 || got[0].State != inbox.StateUnread {
		t.Fatalf("after a new event the done item is %+v, want one unread item", got)
	}
}

// The existing channel_members.muted behaviour must not regress: it is the most
// specific rung of the preference ladder, it already works, and users rely on it.
func TestMutedChannelProducesNoInboxItem(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	h.req(t, http.StatusOK, "PATCH", "/api/v1/channels/"+f.channelID+"/prefs", f.recipient.token,
		map[string]any{"muted": true})

	f.mention(t, h, 3)

	if items := h.inbox(t, f.recipient.token, ""); len(items) != 0 {
		t.Fatalf("a muted channel produced %d inbox items", len(items))
	}
}

// in_app=false means no item at all, not a hidden one. A suppressed notification
// that still counts toward a badge is the worst of both.
func TestNotificationPrefsSuppressFanout(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	h.req(t, http.StatusOK, "PUT", "/api/v1/notification-prefs", f.recipient.token, map[string]any{
		"workspace_id": f.owner.workspaceID,
		"prefs": []map[string]any{
			{"kind": "message.mention", "in_app": false, "push": true, "email": "digest"},
		},
	})

	f.mention(t, h, 2)

	if items := h.inbox(t, f.recipient.token, ""); len(items) != 0 {
		t.Fatalf("in_app=false produced %d inbox items", len(items))
	}

	// And the ladder round-trips through the API.
	r := h.req(t, http.StatusOK, "GET",
		"/api/v1/notification-prefs?workspace_id="+f.owner.workspaceID, f.recipient.token, nil)
	var out struct {
		Prefs []struct {
			Kind  string `json:"kind"`
			InApp bool   `json:"in_app"`
		} `json:"prefs"`
	}
	decodeInto(t, r.Data, &out)
	if len(out.Prefs) != 1 || out.Prefs[0].Kind != "message.mention" || out.Prefs[0].InApp {
		t.Fatalf("stored prefs = %+v", out.Prefs)
	}

	// A malformed kind is a 400 naming the value, not a 500 carrying a CHECK
	// constraint name.
	h.denied(t, http.StatusBadRequest, "PUT", "/api/v1/notification-prefs", f.recipient.token,
		map[string]any{
			"workspace_id": f.owner.workspaceID,
			"prefs":        []map[string]any{{"kind": "NOT VALID", "in_app": true, "push": true, "email": "digest"}},
		})
}

// The four shipped /api/v1/notifications routes keep their paths and their JSON
// shape over the new tables, because the React Native client ships on its own
// schedule. unread-count is the one whose MEANING changed — unread items rather
// than unread events — deliberately; see internal/inbox/compat.go.
func TestLegacyNotificationRoutesProjectTheInbox(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	const burst = 5
	f.mention(t, h, burst)

	r := h.req(t, http.StatusOK, "GET", "/api/v1/notifications", f.recipient.token, nil)
	var legacy []struct {
		ID     string            `json:"id"`
		Type   string            `json:"type"`
		Title  string            `json:"title"`
		Body   string            `json:"body"`
		Data   map[string]string `json:"data"`
		IsRead bool              `json:"is_read"`
	}
	decodeInto(t, r.Data, &legacy)
	if len(legacy) != 1 {
		t.Fatalf("%d legacy notifications, want 1 (coalesced)", len(legacy))
	}
	if legacy[0].Type != "mention" {
		t.Fatalf("legacy type = %q, want one of migration 005's enum values", legacy[0].Type)
	}
	if legacy[0].Data["channel_id"] != f.channelID {
		t.Fatalf("legacy data.channel_id = %q, want %q", legacy[0].Data["channel_id"], f.channelID)
	}
	if legacy[0].IsRead {
		t.Fatal("a fresh notification is not read")
	}

	// The deliberate semantic change: unread ITEMS, not unread events.
	cr := h.req(t, http.StatusOK, "GET", "/api/v1/notifications/unread-count", f.recipient.token, nil)
	var count struct {
		Count int `json:"count"`
	}
	decodeInto(t, cr.Data, &count)
	if count.Count != 1 {
		t.Fatalf("unread-count = %d after %d coalesced events; it now counts ITEMS and must be 1",
			count.Count, burst)
	}

	h.req(t, http.StatusOK, "PUT", "/api/v1/notifications/"+legacy[0].ID+"/read", f.recipient.token, nil)
	if n := h.inboxCount(t, f.recipient.token); n != 0 {
		t.Fatalf("badge = %d after the legacy mark-read route, want 0", n)
	}
}

// Ownership is the whole authorization story for an inbox item: no sharing, no
// admin read, no workspace-admin override. It is enforced in the UPDATE
// predicate, so somebody else's item is a 404 rather than a silent no-op.
func TestInboxItemsAreOwnedNotShared(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)
	f.mention(t, h, 1)

	items := h.inbox(t, f.recipient.token, "")
	if len(items) != 1 {
		t.Fatalf("%d items, want 1", len(items))
	}

	// The workspace OWNER cannot see or mutate the recipient's item.
	if got := h.inbox(t, f.owner.token, ""); len(got) != 0 {
		t.Fatalf("the workspace owner sees %d of somebody else's inbox items", len(got))
	}
	h.denied(t, http.StatusNotFound, "PUT", "/api/v1/inbox/"+items[0].ID+"/read", f.owner.token, nil)
	h.denied(t, http.StatusNotFound, "GET", "/api/v1/inbox/"+items[0].ID+"/events", f.owner.token, nil)
}

// The digest is the only thing that mails anybody about an inbox item, and its
// windows are what collapse a burst into one message. Driving the job body
// directly is the same technique the mail suite uses.
func TestDigestBatchesABurst(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	const burst = 40
	f.mention(t, h, burst)

	queue := &captureMailQueue{}
	renderer := h.mailRenderer(t)
	// QuietPeriod 0 so the burst is immediately eligible; the window itself is
	// unit-tested, what is integrated here is the grouping and the claim.
	d := inbox.NewDigester(inbox.NewRepository(h.app.DB), renderer, queue,
		inbox.DigestConfig{QuietPeriod: time.Nanosecond, MinInterval: time.Hour}, logger.New("error"))

	sent, err := d.Run(context.Background())
	if err != nil {
		t.Fatalf("digest run: %v", err)
	}
	if sent < 1 {
		t.Fatalf("digest queued %d messages, want at least 1", sent)
	}
	mine := queue.forRecipient(f.recipient.email)
	if len(mine) != 1 {
		t.Fatalf("%d digests for the recipient, want exactly 1 for a burst of %d", len(mine), burst)
	}
	if mine[0].kind != "digest" {
		t.Fatalf("digest queued as kind %q", mine[0].kind)
	}

	// notified_at is now set, so a second run produces nothing for this user —
	// which is the claim doing its job.
	before := len(queue.forRecipient(f.recipient.email))
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("second digest run: %v", err)
	}
	if got := len(queue.forRecipient(f.recipient.email)); got != before {
		t.Fatalf("a second digest run mailed the same items again (%d -> %d)", before, got)
	}
}

// email=never means the items are claimed (so they are not reconsidered every
// five minutes forever) but nothing is mailed.
func TestDigestHonoursEmailNever(t *testing.T) {
	h := getHarness(t)
	f := newMentionFixture(t, h)

	h.req(t, http.StatusOK, "PUT", "/api/v1/notification-prefs", f.recipient.token, map[string]any{
		"workspace_id": f.owner.workspaceID,
		"prefs":        []map[string]any{{"kind": "*", "in_app": true, "push": true, "email": "never"}},
	})
	f.mention(t, h, 3)

	queue := &captureMailQueue{}
	d := inbox.NewDigester(inbox.NewRepository(h.app.DB), h.mailRenderer(t), queue,
		inbox.DigestConfig{QuietPeriod: time.Nanosecond, MinInterval: time.Hour}, logger.New("error"))
	if _, err := d.Run(context.Background()); err != nil {
		t.Fatalf("digest run: %v", err)
	}
	if got := queue.forRecipient(f.recipient.email); len(got) != 0 {
		t.Fatalf("email=never still produced %d digests", len(got))
	}
}

// captureMailQueue stands in for mail.Publisher: it records what the digest job
// decided to send rather than putting it on the stream, which is what the
// assertions above are actually about. mail_test.go covers the queue itself.
type captureMailQueue struct {
	mu   sync.Mutex
	sent []queuedDigest
}

type queuedDigest struct {
	workspaceID string
	kind        string
	msg         *mail.Message
}

func (q *captureMailQueue) Queue(_ context.Context, workspaceID, kind string, msg *mail.Message) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.sent = append(q.sent, queuedDigest{workspaceID: workspaceID, kind: kind, msg: msg})
	return nil
}

func (q *captureMailQueue) forRecipient(email string) []queuedDigest {
	q.mu.Lock()
	defer q.mu.Unlock()
	var out []queuedDigest
	for _, d := range q.sent {
		for _, to := range d.msg.To {
			if strings.EqualFold(to.Email, email) {
				out = append(out, d)
			}
		}
	}
	return out
}

// mailRenderer builds the same renderer app.New builds, so the digest templates
// are the ones that ship rather than a stub.
func (h *harness) mailRenderer(t *testing.T) *mail.Renderer {
	t.Helper()
	r, err := mail.NewRenderer(mail.RendererConfig{
		BaseURL:     h.app.Config.Mail.PublicBaseURL,
		ProductName: h.app.Config.Mail.ProductName,
	})
	if err != nil {
		t.Fatalf("build mail renderer: %v", err)
	}
	return r
}
