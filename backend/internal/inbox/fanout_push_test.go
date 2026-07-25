package inbox_test

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/inbox"
	"github.com/Wick-Lim/SuperOps/backend/internal/notification"
	"github.com/Wick-Lim/SuperOps/backend/internal/push"
	"github.com/Wick-Lim/SuperOps/backend/pkg/httputil"
)

// capturePusher stands in for push.Dispatcher. It records what the fan-out
// decided to send rather than sending it — there is no Expo project and no
// device in this environment, so delivery itself is out of reach; what is in
// reach, and what actually goes wrong, is *who* gets addressed and *how often*.
type capturePusher struct {
	mu     sync.Mutex
	pushed []push.Message
}

func (p *capturePusher) Enqueue(msgs []push.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.pushed = append(p.pushed, msgs...)
}

func (p *capturePusher) sent() []push.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]push.Message(nil), p.pushed...)
}

// stubDevices is the device address book. Backed by a map so a test can give one
// user two handsets and another none.
type stubDevices map[string][]string

func (d stubDevices) PushTokensForUser(_ context.Context, userID string) ([]string, error) {
	return d[userID], nil
}

type dmFixture struct {
	pool    *pgxpool.Pool
	svc     *notification.Service
	repo    *inbox.Repository
	pusher  *capturePusher
	devices stubDevices

	workspaceID string
	channelID   string
	sender      string
	recipient   string
	messageID   string
	subject     string
}

var fixtureSeq int

// newDMFixture builds a real two-person DM: two users, a workspace they both
// belong to, a dm channel they are both members of. Each call makes fresh rows
// so tests cannot see each other's items.
func newDMFixture(t *testing.T) *dmFixture {
	t.Helper()
	pool := testDB(t)
	ctx := t.Context()

	fixtureSeq++
	seq := fixtureSeq

	id := func() string {
		t.Helper()
		var out string
		if err := pool.QueryRow(ctx, `SELECT uuid_generate_v4()`).Scan(&out); err != nil {
			t.Fatalf("generate uuid: %v", err)
		}
		return out
	}

	f := &dmFixture{
		pool:        pool,
		repo:        inbox.NewRepository(pool),
		devices:     stubDevices{},
		pusher:      &capturePusher{},
		workspaceID: id(),
		channelID:   id(),
		sender:      id(),
		recipient:   id(),
		messageID:   id(),
	}
	f.subject = "superops." + f.workspaceID + ".message.created"

	for i, uid := range []string{f.sender, f.recipient} {
		name := fmt.Sprintf("push-%d-%d", seq, i)
		if _, err := pool.Exec(ctx,
			`INSERT INTO users (id, email, username, full_name, is_active)
			 VALUES ($1, $2, $3, $4, TRUE)`,
			uid, name+"@test.local", name, name); err != nil {
			t.Fatalf("insert user: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO workspaces (id, name, slug, owner_id) VALUES ($1, $2, $3, $4)`,
		f.workspaceID, fmt.Sprintf("ws-%d", seq), fmt.Sprintf("ws-%d", seq), f.sender); err != nil {
		t.Fatalf("insert workspace: %v", err)
	}
	for _, uid := range []string{f.sender, f.recipient} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO workspace_members (workspace_id, user_id, role) VALUES ($1, $2, 'member')`,
			f.workspaceID, uid); err != nil {
			t.Fatalf("insert workspace member: %v", err)
		}
	}
	if _, err := pool.Exec(ctx,
		`INSERT INTO channels (id, workspace_id, type, creator_id) VALUES ($1, $2, 'dm', $3)`,
		f.channelID, f.workspaceID, f.sender); err != nil {
		t.Fatalf("insert channel: %v", err)
	}
	for _, uid := range []string{f.sender, f.recipient} {
		if _, err := pool.Exec(ctx,
			`INSERT INTO channel_members (channel_id, user_id, role) VALUES ($1, $2, 'admin')`,
			f.channelID, uid); err != nil {
			t.Fatalf("insert channel member: %v", err)
		}
	}

	f.rewire(f.devices, f.pusher)
	return f
}

// rewire rebuilds the producer over a given push pipeline. nats is nil: the
// realtime relay is not what these tests are about, and Notifier.relay already
// skips it when there is no client.
func (f *dmFixture) rewire(devices inbox.DeviceTokenLister, pusher inbox.Pusher) {
	quiet := slog.New(slog.NewTextHandler(io.Discard, nil))
	notifier := inbox.NewNotifier(f.repo, nil, devices, pusher, quiet)
	f.svc = notification.NewService(f.pool, authz.New(f.pool), notifier, quiet)
}

// deliver replays one message.created event, as the worker's durable consumer
// would.
func (f *dmFixture) deliver(t *testing.T) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"type":"message.new","data":{"id":%q,"channel_id":%q,"user_id":%q,"content":"hello there"}}`,
		f.messageID, f.channelID, f.sender)
	msg := &nats.Msg{Subject: f.subject, Data: []byte(payload)}
	if err := f.svc.HandleMessage(t.Context(), msg); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
}

// item reads back the one inbox item this DM produces for a user, or nil.
func (f *dmFixture) item(t *testing.T, userID string) *inbox.Item {
	t.Helper()
	id := inbox.ItemID(userID, inbox.SubjectChannel, f.channelID)
	it, err := f.repo.GetItem(t.Context(), id, userID)
	if err != nil {
		t.Fatalf("get inbox item: %v", err)
	}
	return it
}

func (f *dmFixture) eventRows(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM inbox_events WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count inbox events: %v", err)
	}
	return n
}

// THE test. The worker's consumer is at-least-once, so a lost ack or a nak
// replays the whole fan-out. With a coalesced counter, a replay that is not
// gated does not produce an invisible duplicate row — it produces a badge that
// is wrong by one, permanently, with nothing to correct it from.
//
// The gate is `INSERT ... ON CONFLICT (id) DO NOTHING RETURNING user_id` on
// inbox_events, in the same transaction as the item upsert. This asserts both
// halves: the count moves exactly once, and nothing buzzes twice.
func TestRedeliveryMovesTheCounterExactlyOnce(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]", "ExponentPushToken[tablet]"}

	for range 5 {
		f.deliver(t)
	}

	if n := f.eventRows(t, f.recipient); n != 1 {
		t.Fatalf("%d inbox_events rows after 5 deliveries, want 1", n)
	}
	it := f.item(t, f.recipient)
	if it == nil {
		t.Fatal("no inbox item was created")
	}
	if it.UnreadCount != 1 || it.EventCount != 1 {
		t.Fatalf("unread_count/event_count = %d/%d after 5 deliveries, want 1/1",
			it.UnreadCount, it.EventCount)
	}

	sent := f.pusher.sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d pushes after 5 deliveries, want 2 (one per device, once)", len(sent))
	}
	got := map[string]int{}
	for _, m := range sent {
		got[m.Token]++
	}
	for _, want := range []string{"ExponentPushToken[phone]", "ExponentPushToken[tablet]"} {
		if got[want] != 1 {
			t.Fatalf("token %s pushed %d times, want 1", want, got[want])
		}
	}
}

// A burst is one row carrying a count, not N rows. This is what makes the badge
// comparable to the list: forty mentions in one channel is one thing that needs
// attention, not forty.
func TestBurstCoalescesIntoOneItem(t *testing.T) {
	f := newDMFixture(t)

	const burst = 40
	for range burst {
		f.messageID = uuid.NewString()
		f.deliver(t)
	}

	if n := f.eventRows(t, f.recipient); n != burst {
		t.Fatalf("%d inbox_events rows, want %d", n, burst)
	}
	it := f.item(t, f.recipient)
	if it == nil {
		t.Fatal("no inbox item was created")
	}
	if it.UnreadCount != burst || it.EventCount != burst {
		t.Fatalf("unread_count/event_count = %d/%d, want %d/%d",
			it.UnreadCount, it.EventCount, burst, burst)
	}

	// One row in the list, and one on the badge.
	items, err := f.repo.ListItems(t.Context(), inbox.ListFilter{UserID: f.recipient}, httputil.Cursor{}, 50)
	if err != nil {
		t.Fatalf("list items: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("%d items in the inbox after a burst of %d, want 1", len(items), burst)
	}
	badge, err := f.repo.UnreadItems(t.Context(), f.recipient)
	if err != nil {
		t.Fatalf("unread items: %v", err)
	}
	if badge != 1 {
		t.Fatalf("badge = %d after a burst of %d, want 1", badge, burst)
	}
}

// A done item that receives a new event is not done any more — otherwise
// "archive" would silently swallow every future message in that conversation.
func TestDoneReopensOnNewEvent(t *testing.T) {
	f := newDMFixture(t)
	f.deliver(t)

	it := f.item(t, f.recipient)
	if it == nil {
		t.Fatal("no inbox item")
	}
	if ok, err := f.repo.SetState(t.Context(), it.ID, f.recipient, inbox.StateDone); err != nil || !ok {
		t.Fatalf("SetState(done) = %v, %v", ok, err)
	}
	if got := f.item(t, f.recipient); got.State != inbox.StateDone {
		t.Fatalf("state = %q after done, want done", got.State)
	}

	f.messageID = uuid.NewString()
	f.deliver(t)

	got := f.item(t, f.recipient)
	if got.State != inbox.StateUnread {
		t.Fatalf("state = %q after a new event on a done item, want unread", got.State)
	}
	if got.DoneAt != nil {
		t.Fatal("done_at survived a reopen")
	}
}

// The author must never be pushed their own message, and a user with no
// registered device must not produce an empty enqueue.
func TestFanOutDoesNotPushTheAuthorOrDevicelessUsers(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.sender] = []string{"ExponentPushToken[authors-own-phone]"}
	// The recipient has registered nothing.

	f.deliver(t)

	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("sent %d pushes, want none: %+v", len(sent), sent)
	}
	if n := f.eventRows(t, f.sender); n != 0 {
		t.Fatalf("the author got %d inbox events for their own message", n)
	}
}

// muted is applied by the producer's fan-out gate, before internal/inbox ever
// sees the recipient — so a muted channel produces neither an item nor a push.
// The point of asserting it here is that the rewiring onto inbox.Notifier did
// not move that gate somewhere it can be bypassed.
func TestMutedChannelProducesNoInboxItem(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE channel_members SET muted = TRUE WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.recipient); err != nil {
		t.Fatalf("mute channel: %v", err)
	}

	f.deliver(t)

	if n := f.eventRows(t, f.recipient); n != 0 {
		t.Fatalf("a muted channel produced %d inbox events", n)
	}
	if f.item(t, f.recipient) != nil {
		t.Fatal("a muted channel produced an inbox item")
	}
	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("a muted channel produced %d pushes", len(sent))
	}
}

func TestNotificationPrefMentionsOnlyStillSuppressesADM(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE channel_members SET notification_pref = 'mentions' WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.recipient); err != nil {
		t.Fatalf("set notification_pref: %v", err)
	}

	f.deliver(t)

	if n := f.eventRows(t, f.recipient); n != 0 {
		t.Fatalf("notification_pref=mentions still produced %d events for a DM", n)
	}
	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("notification_pref=mentions still produced %d pushes", len(sent))
	}
}

// in_app=false means NO ITEM AT ALL, push=false means an item with no buzz. A
// suppressed notification that still counts toward a badge is the worst of both.
func TestNotificationPrefsSuppressFanout(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	// push=false: item, no push.
	if err := f.repo.ReplacePrefs(t.Context(), f.recipient, f.workspaceID,
		[]inbox.PrefRow{{Kind: "message.*", InApp: true, Push: false, Email: inbox.EmailDigest}}); err != nil {
		t.Fatalf("replace prefs: %v", err)
	}
	f.deliver(t)
	if f.item(t, f.recipient) == nil {
		t.Fatal("push=false suppressed the item as well as the push")
	}
	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("push=false still produced %d pushes", len(sent))
	}

	// in_app=false: nothing at all, on a fresh subject.
	f2 := newDMFixture(t)
	f2.devices[f2.recipient] = []string{"ExponentPushToken[phone]"}
	if err := f2.repo.ReplacePrefs(t.Context(), f2.recipient, f2.workspaceID,
		[]inbox.PrefRow{{Kind: "*", InApp: false, Push: true, Email: inbox.EmailNever}}); err != nil {
		t.Fatalf("replace prefs: %v", err)
	}
	f2.deliver(t)
	if n := f2.eventRows(t, f2.recipient); n != 0 {
		t.Fatalf("in_app=false produced %d events", n)
	}
	if sent := f2.pusher.sent(); len(sent) != 0 {
		t.Fatalf("in_app=false produced %d pushes", len(sent))
	}
}

// Deregistering the device is the opt-out that exists, and it has to be
// immediate: the client calls it on logout, and until it takes effect the next
// person to sign in on that handset would keep receiving the previous user's
// notifications.
func TestDeregisteringADeviceStopsItsPushes(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	f.deliver(t)
	if n := len(f.pusher.sent()); n != 1 {
		t.Fatalf("sent %d pushes before deregistration, want 1", n)
	}

	delete(f.devices, f.recipient)
	f.messageID = uuid.NewString()
	f.deliver(t)

	if n := len(f.pusher.sent()); n != 1 {
		t.Fatalf("sent %d pushes in total, want 1 — a deregistered device was still addressed", n)
	}
	if n := f.eventRows(t, f.recipient); n != 2 {
		t.Fatalf("%d inbox events, want 2 — deregistering a device must not suppress in-app items", n)
	}
}

// The payload is what makes a tap open the right conversation, and the badge is
// what keeps the app icon right while the app is not running at all.
//
// `notification_id` is still in the payload under its old name: the shipped
// React Native client reads exactly that key (app/src/lib/push.ts), so dropping
// it would break deep links on every handset that has not updated.
func TestPushCarriesDeepLinkDataAndBadge(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	f.deliver(t)

	sent := f.pusher.sent()
	if len(sent) != 1 {
		t.Fatalf("sent %d pushes, want 1", len(sent))
	}
	m := sent[0]
	if m.Title != "New message" || m.Body != "hello there" {
		t.Fatalf("title/body = %q/%q", m.Title, m.Body)
	}
	if m.Data["channel_id"] != f.channelID {
		t.Fatalf("data.channel_id = %q, want %q", m.Data["channel_id"], f.channelID)
	}
	if m.Data["message_id"] != f.messageID {
		t.Fatalf("data.message_id = %q, want %q", m.Data["message_id"], f.messageID)
	}
	if m.Data["type"] != "dm" {
		t.Fatalf("data.type = %q, want the legacy enum value %q", m.Data["type"], "dm")
	}
	if m.Data["kind"] != inbox.KindMessageDM {
		t.Fatalf("data.kind = %q, want %q", m.Data["kind"], inbox.KindMessageDM)
	}
	wantItem := inbox.ItemID(f.recipient, inbox.SubjectChannel, f.channelID)
	if m.Data["notification_id"] != wantItem || m.Data["item_id"] != wantItem {
		t.Fatalf("data.notification_id/item_id = %q/%q, want %q",
			m.Data["notification_id"], m.Data["item_id"], wantItem)
	}
	if m.Badge == nil || *m.Badge != 1 {
		t.Fatalf("badge = %v, want 1 (the recipient's unread ITEM count)", m.Badge)
	}
}

// PUSH_ENABLED off is the default, and it must be a plain no-op rather than a
// nil dereference somewhere inside the fan-out.
func TestFanOutWithoutAPusherStillNotifies(t *testing.T) {
	f := newDMFixture(t)
	f.rewire(nil, nil)

	f.deliver(t)

	if n := f.eventRows(t, f.recipient); n != 1 {
		t.Fatalf("%d inbox events with push disabled, want 1", n)
	}
}

// The reconciler is the watchdog on denormalized state that answers a question
// the user asked out loud. Drift is injected the way it would actually happen —
// a counter that moved without an event behind it — and the job has to both see
// it and repair it.
func TestReconcileRepairsInjectedDrift(t *testing.T) {
	f := newDMFixture(t)
	f.deliver(t)

	it := f.item(t, f.recipient)
	if it == nil {
		t.Fatal("no inbox item")
	}
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE inbox_items SET unread_count = 99, event_count = 99 WHERE id = $1`, it.ID); err != nil {
		t.Fatalf("inject drift: %v", err)
	}

	drifts, repaired, err := f.repo.ReconcileUsers(t.Context(), []string{f.recipient}, 20)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if repaired != 1 {
		t.Fatalf("repaired %d items, want 1", repaired)
	}
	if len(drifts) != 1 || drifts[0].StoredCount != 99 || drifts[0].ActualCount != 1 {
		t.Fatalf("drift report = %+v, want one entry 99 -> 1", drifts)
	}

	after := f.item(t, f.recipient)
	if after.EventCount != 1 || after.UnreadCount != 1 {
		t.Fatalf("after repair: event_count/unread_count = %d/%d, want 1/1",
			after.EventCount, after.UnreadCount)
	}

	// A second run finds nothing: the repair is idempotent and the job does not
	// keep reporting the same drift forever.
	_, repaired, err = f.repo.ReconcileUsers(t.Context(), []string{f.recipient}, 20)
	if err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if repaired != 0 {
		t.Fatalf("second reconcile repaired %d items, want 0", repaired)
	}
}

// An item whose events have all been deleted (retention, a workspace teardown)
// cannot be unread — there is nothing left to read. That is the one case where
// the reconciler is allowed to change state, and it is a structural fact rather
// than a guess about what the user meant.
func TestReconcileClosesOrphanedItems(t *testing.T) {
	f := newDMFixture(t)
	f.deliver(t)

	if _, err := f.pool.Exec(t.Context(),
		`DELETE FROM inbox_events WHERE user_id = $1`, f.recipient); err != nil {
		t.Fatalf("delete events: %v", err)
	}

	drifts, repaired, err := f.repo.ReconcileUsers(t.Context(), []string{f.recipient}, 20)
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if repaired != 1 || len(drifts) != 1 || !drifts[0].Orphan {
		t.Fatalf("drift report = %+v (repaired %d), want one orphan", drifts, repaired)
	}
	if got := f.item(t, f.recipient); got.State != inbox.StateRead || got.EventCount != 0 {
		t.Fatalf("orphaned item: state=%q event_count=%d, want read/0", got.State, got.EventCount)
	}
	badge, err := f.repo.UnreadItems(t.Context(), f.recipient)
	if err != nil {
		t.Fatalf("unread items: %v", err)
	}
	if badge != 0 {
		t.Fatalf("badge = %d over an item with no events, want 0", badge)
	}
}

// The mechanism the backfill's digest guard relies on: an item with notified_at
// set is not a digest candidate. migration_test.go asserts the migration sets
// it; this asserts that setting it actually suppresses the mail.
func TestNotifiedItemsAreNotDigestCandidates(t *testing.T) {
	f := newDMFixture(t)
	f.deliver(t)

	repo := f.repo
	// Eligible: unread, never mailed, quiet.
	got, err := repo.DigestCandidates(t.Context(), time.Nanosecond, time.Hour, 100)
	if err != nil {
		t.Fatalf("digest candidates: %v", err)
	}
	if !containsUser(got, f.recipient) {
		t.Fatal("a fresh unread item is not a digest candidate")
	}

	// What the backfill does to every row it carries.
	if _, err := f.pool.Exec(t.Context(),
		`UPDATE inbox_items SET notified_at = NOW() WHERE user_id = $1`, f.recipient); err != nil {
		t.Fatalf("set notified_at: %v", err)
	}

	got, err = repo.DigestCandidates(t.Context(), time.Nanosecond, time.Hour, 100)
	if err != nil {
		t.Fatalf("digest candidates: %v", err)
	}
	if containsUser(got, f.recipient) {
		t.Fatal("an item with notified_at set is still a digest candidate; " +
			"the backfill's storm guard does nothing")
	}
}

// The quiet period is what collapses a burst: an item still receiving events is
// not yet mailed.
func TestQuietPeriodDefersAnActiveItem(t *testing.T) {
	f := newDMFixture(t)
	f.deliver(t)

	got, err := f.repo.DigestCandidates(t.Context(), time.Hour, time.Hour, 100)
	if err != nil {
		t.Fatalf("digest candidates: %v", err)
	}
	if containsUser(got, f.recipient) {
		t.Fatal("an item that arrived a moment ago is already a digest candidate " +
			"with a one-hour quiet period")
	}
}

func containsUser(cs []inbox.DigestCandidate, userID string) bool {
	for _, c := range cs {
		if c.UserID == userID {
			return true
		}
	}
	return false
}
