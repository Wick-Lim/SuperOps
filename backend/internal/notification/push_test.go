package notification

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/push"
)

// capturePusher stands in for push.Dispatcher. It records what the fan-out
// decided to send rather than sending it — there is no Expo project and no
// device in this environment, so delivery itself is out of reach; what is in
// reach, and what actually goes wrong, is *who* gets addressed and *how often*.
type capturePusher struct {
	mu     sync.Mutex
	calls  [][]push.Message
	pushed []push.Message
}

func (p *capturePusher) Enqueue(msgs []push.Message) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.calls = append(p.calls, append([]push.Message(nil), msgs...))
	p.pushed = append(p.pushed, msgs...)
}

func (p *capturePusher) sent() []push.Message {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]push.Message(nil), p.pushed...)
}

// stubDevices is the device address book. Backed by a map so a test can give
// one user two handsets and another none.
type stubDevices map[string][]string

func (d stubDevices) PushTokensForUser(_ context.Context, userID string) ([]string, error) {
	return d[userID], nil
}

type dmFixture struct {
	pool    *pgxpool.Pool
	svc     *Service
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
// so tests cannot see each other's notifications.
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

	// nats is nil: the realtime relay is not what these tests are about, and
	// createAndPush already skips it when there is no client.
	f.svc = NewService(NewRepository(pool), pool, authz.New(pool), nil,
		f.devices, f.pusher, slog.New(slog.NewTextHandler(io.Discard, nil)))
	return f
}

// deliver replays one message.created event, as the worker's durable consumer
// would.
func (f *dmFixture) deliver(t *testing.T) {
	t.Helper()
	payload := fmt.Sprintf(
		`{"type":"message.new","data":{"id":%q,"channel_id":%q,"user_id":%q,"content":"hello there"}}`,
		f.messageID, f.channelID, f.sender)
	if err := f.svc.HandleMessage(t.Context(), event(f.subject, payload)); err != nil {
		t.Fatalf("HandleMessage: %v", err)
	}
}

func (f *dmFixture) notificationRows(t *testing.T, userID string) int {
	t.Helper()
	var n int
	if err := f.pool.QueryRow(t.Context(),
		`SELECT COUNT(*) FROM notifications WHERE user_id = $1`, userID).Scan(&n); err != nil {
		t.Fatalf("count notifications: %v", err)
	}
	return n
}

// ON CONFLICT DO NOTHING reporting zero affected rows is the entire mechanism
// behind push idempotency, so it is asserted directly rather than inferred.
func TestCreateReportsWhetherTheRowWasInserted(t *testing.T) {
	f := newDMFixture(t)
	repo := NewRepository(f.pool)

	n := &Notification{
		ID:     notificationID(TypeDM, f.recipient, f.messageID),
		UserID: f.recipient,
		Type:   TypeDM,
		Title:  "New message",
		Body:   "hello",
		Data:   `{}`,
	}

	created, err := repo.Create(t.Context(), n)
	if err != nil {
		t.Fatalf("first Create: %v", err)
	}
	if !created {
		t.Fatal("the first insert of a notification must report created=true")
	}

	created, err = repo.Create(t.Context(), n)
	if err != nil {
		t.Fatalf("second Create: %v", err)
	}
	if created {
		t.Fatal("re-inserting the same notification must report created=false")
	}
}

// The failure this prevents: the worker's consumer is at-least-once, so a lost
// ack or a nak replays the whole fan-out. A duplicate row is invisible (the id
// is derived), but a duplicate push is a second buzz in the recipient's pocket
// for a message they have already read.
func TestFanOutPushesOnceAcrossRedeliveries(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]", "ExponentPushToken[tablet]"}

	for range 3 {
		f.deliver(t)
	}

	if n := f.notificationRows(t, f.recipient); n != 1 {
		t.Fatalf("%d notification rows after 3 deliveries, want 1", n)
	}
	sent := f.pusher.sent()
	if len(sent) != 2 {
		t.Fatalf("sent %d pushes after 3 deliveries, want 2 (one per device, once)", len(sent))
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
	if n := f.notificationRows(t, f.sender); n != 0 {
		t.Fatalf("the author got %d notifications of their own message", n)
	}
}

// muted is applied by the shared fan-out gate, before anything is created — so
// a muted channel produces neither an in-app notification nor a push. The point
// of asserting it here is that push rides that same gate rather than bypassing
// it with its own audience query.
func TestPushObeysTheSameMuteGateAsTheNotification(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE channel_members SET muted = TRUE WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.recipient); err != nil {
		t.Fatalf("mute channel: %v", err)
	}

	f.deliver(t)

	if n := f.notificationRows(t, f.recipient); n != 0 {
		t.Fatalf("a muted channel produced %d notification rows", n)
	}
	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("a muted channel produced %d pushes", len(sent))
	}
}

func TestPushObeysNotificationPrefMentionsOnly(t *testing.T) {
	f := newDMFixture(t)
	f.devices[f.recipient] = []string{"ExponentPushToken[phone]"}

	if _, err := f.pool.Exec(t.Context(),
		`UPDATE channel_members SET notification_pref = 'mentions' WHERE channel_id = $1 AND user_id = $2`,
		f.channelID, f.recipient); err != nil {
		t.Fatalf("set notification_pref: %v", err)
	}

	f.deliver(t)

	// A plain DM is not a mention.
	if n := f.notificationRows(t, f.recipient); n != 0 {
		t.Fatalf("notification_pref=mentions still produced %d rows for a DM", n)
	}
	if sent := f.pusher.sent(); len(sent) != 0 {
		t.Fatalf("notification_pref=mentions still produced %d pushes", len(sent))
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
	// A different message. Replacing the last character with a fixed one was a
	// 1-in-16 flake: a uuid already ending in that character produced the SAME
	// id, the delivery deduped, and the assertion below failed for a reason
	// unrelated to what it tests. Flip to a character the original is not.
	f.messageID = differentID(f.messageID)
	f.deliver(t)

	if n := len(f.pusher.sent()); n != 1 {
		t.Fatalf("sent %d pushes in total, want 1 — a deregistered device was still addressed", n)
	}
	if n := f.notificationRows(t, f.recipient); n != 2 {
		t.Fatalf("%d notification rows, want 2 — deregistering a device must not suppress in-app notifications", n)
	}
}

// The payload is what makes a tap open the right conversation, and the badge is
// what keeps the app icon right while the app is not running at all.
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
	if m.Data["type"] != string(TypeDM) {
		t.Fatalf("data.type = %q, want %q", m.Data["type"], TypeDM)
	}
	if m.Data["notification_id"] != notificationID(TypeDM, f.recipient, f.messageID) {
		t.Fatalf("data.notification_id = %q", m.Data["notification_id"])
	}
	if m.Badge == nil || *m.Badge != 1 {
		t.Fatalf("badge = %v, want 1 (the recipient's unread count)", m.Badge)
	}
}

// PUSH_ENABLED off is the default, and it must be a plain no-op rather than a
// nil dereference somewhere inside the fan-out.
func TestFanOutWithoutAPusherStillNotifies(t *testing.T) {
	f := newDMFixture(t)
	f.svc = NewService(NewRepository(f.pool), f.pool, authz.New(f.pool), nil,
		nil, nil, slog.New(slog.NewTextHandler(io.Discard, nil)))

	f.deliver(t)

	if n := f.notificationRows(t, f.recipient); n != 1 {
		t.Fatalf("%d notification rows with push disabled, want 1", n)
	}
}

// differentID returns an id that is guaranteed not to equal in, by flipping the
// last character between two values rather than assigning a fixed one.
//
// The fixed-character version was a 1-in-16 flake: a uuid that already ended in
// that character was left unchanged, so a test meaning "now deliver a different
// message" delivered the same one, hit the idempotency guard, and failed with a
// message that pointed at deduplication rather than at the test's own bug. It
// only ever appeared with a reachable Postgres, which is why the suite looked
// stable.
func differentID(in string) string {
	if in == "" {
		return "0"
	}
	last := in[len(in)-1]
	next := byte('0')
	if last == '0' {
		next = '1'
	}
	return in[:len(in)-1] + string(next)
}
