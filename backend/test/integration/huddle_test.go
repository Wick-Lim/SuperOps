//go:build integration

package integration

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/huddle"
)

// The huddle lifecycle is tested against real Postgres and NO media server.
//
// That split is deliberate and it is the honest one: the media server is a
// ROADMAP §3c deployment-dependent capability that CI does not have, and
// pretending otherwise would mean a fake SFU whose behaviour is whatever this
// test assumes. What IS testable without one — and what every real failure in
// this feature has been — is the database half: the race when two people click
// at once, redelivery, and the reconciler's repair. internal/rtc's own tests
// cover the token and the signature, which need no server either.

func huddleRepo(t *testing.T) *huddle.Repository {
	t.Helper()
	return huddle.NewRepository(getHarness(t).app.DB)
}

// TWO PEOPLE CLICK HUDDLE AT THE SAME MOMENT.
//
// Without the partial unique index both INSERTs succeed, the two clients get
// different room names, and neither can hear the other — which presents as a
// network fault and is a missing index. This drives the real repository from
// several goroutines against real Postgres, because the failure only exists
// under concurrency.
func TestConcurrentHuddleStartsLandInOneRoom(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-race"))
	repo := huddleRepo(t)

	const racers = 8
	ids := make([]string, racers)
	creates := make([]bool, racers)
	errs := make([]error, racers)

	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			<-start
			hud, created, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
			errs[i] = err
			if err == nil {
				ids[i] = hud.ID
				creates[i] = created
			}
		}(i)
	}
	close(start)
	wg.Wait()

	createdCount := 0
	for i := range ids {
		if errs[i] != nil {
			t.Fatalf("racer %d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("racer %d landed in huddle %s, racer 0 in %s — they cannot hear each other",
				i, ids[i], ids[0])
		}
		if creates[i] {
			createdCount++
		}
	}
	// EXACTLY ONE creator. The caller uses this to decide whether to create the
	// media room and whether to announce the call; two would mean two rooms and
	// two announcements.
	if createdCount != 1 {
		t.Fatalf("%d racers believed they created the huddle; the media room would be created "+
			"that many times and the channel would see that many 'started' frames", createdCount)
	}

	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, _ = repo.End(ctx, ids[0], "ended_by_user")
	})
}

// A channel can start a NEW call once the old one ended. The partial index is
// on live rows only, and getting that wrong would make a channel usable for
// exactly one call ever.
func TestAChannelCanStartAnotherCallAfterTheFirstEnds(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-again"))
	repo := huddleRepo(t)
	ctx := context.Background()

	first, created, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil || !created {
		t.Fatalf("first start: %v created=%v", err, created)
	}
	if ended, err := repo.End(ctx, first.ID, "ended_by_user"); err != nil || !ended {
		t.Fatalf("end: %v ended=%v", err, ended)
	}

	second, created, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil || !created {
		t.Fatalf("second start: %v created=%v", err, created)
	}
	if second.ID == first.ID {
		t.Fatal("the second call reused the first huddle")
	}
	// A FRESH ROOM NAME. Reusing it would make the new call inherit the media
	// state — and the participant list — of the one that just ended.
	if second.RoomName == first.RoomName {
		t.Fatal("the second call reuses the first room name, so it inherits its state")
	}
	t.Cleanup(func() { _, _ = repo.End(context.Background(), second.ID, "ended_by_user") })
}

// Ending twice must not produce two reasons. The reconciler and a webhook can
// race for the same call, and whichever loses has to be a no-op rather than an
// overwrite.
func TestEndingTwiceKeepsTheFirstReason(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-end-twice"))
	repo := huddleRepo(t)
	ctx := context.Background()

	hud, _, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil {
		t.Fatal(err)
	}
	if ended, _ := repo.End(ctx, hud.ID, "ended_by_user"); !ended {
		t.Fatal("the first end did not apply")
	}
	if ended, _ := repo.End(ctx, hud.ID, "reconciled"); ended {
		t.Fatal("the second end applied, so the reason a user sees depends on a race")
	}
	after, err := repo.ByID(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.EndedReason == nil || *after.EndedReason != "ended_by_user" {
		t.Fatalf("ended_reason = %v, want ended_by_user", after.EndedReason)
	}
}

// REDELIVERY IS NOT OPTIONAL. The media server delivers at least once and
// retries; without the dedupe table a retried participant_joined re-opens a
// session that already ended, and the roster shows somebody who hung up ten
// minutes ago.
func TestWebhookRedeliveryIsIdempotent(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-redeliver"))
	repo := huddleRepo(t)
	ctx := context.Background()

	hud, _, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = repo.End(context.Background(), hud.ID, "ended_by_user") })

	join := huddle.Event{
		ID:   fmt.Sprintf("ev-join-%d", time.Now().UnixNano()),
		Type: "participant_joined", Room: hud.RoomName, SID: "sid-1", Identity: me,
	}
	leave := huddle.Event{
		ID:   fmt.Sprintf("ev-leave-%d", time.Now().UnixNano()),
		Type: "participant_left", Room: hud.RoomName, SID: "sid-1",
	}

	for i := 0; i < 3; i++ {
		if err := repo.Apply(ctx, join); err != nil {
			t.Fatalf("apply join %d: %v", i, err)
		}
	}
	live, err := repo.Participants(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("three deliveries of one join produced %d live participants", len(live))
	}

	if err := repo.Apply(ctx, leave); err != nil {
		t.Fatal(err)
	}
	// THE DANGEROUS REPLAY: the join arrives again AFTER the leave. Without the
	// dedupe table it re-opens the session and the person is back in the call
	// without having done anything.
	if err := repo.Apply(ctx, join); err != nil {
		t.Fatal(err)
	}
	live, err = repo.Participants(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatal("a replayed join re-opened a session that had already ended; somebody who hung " +
			"up is shown as still in the call")
	}
}

// An event with no id cannot be made safe to redeliver, so it is REFUSED rather
// than applied once and hoped about.
func TestWebhookWithoutAnIDIsRefused(t *testing.T) {
	repo := huddleRepo(t)
	err := repo.Apply(context.Background(), huddle.Event{Type: "participant_joined", Room: "hd_x"})
	if err == nil {
		t.Fatal("applied an event with no idempotency key")
	}
}

// An event for a room this deployment does not know about is IGNORED, not
// failed: the media server may host rooms we did not create, and a 500 makes it
// retry forever.
func TestWebhookForAnUnknownRoomIsIgnored(t *testing.T) {
	repo := huddleRepo(t)
	err := repo.Apply(context.Background(), huddle.Event{
		ID:   fmt.Sprintf("ev-unknown-%d", time.Now().UnixNano()),
		Type: "participant_joined", Room: "hd_not_ours", SID: "s", Identity: "u",
	})
	if err != nil {
		t.Fatalf("an event for an unknown room failed instead of being ignored: %v", err)
	}
}

// room_finished closes the call AND every session still open in it. Leaving the
// sessions open would make the roster of a finished call keep listing everybody
// who was ever in it.
func TestRoomFinishedClosesEverySession(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-finish"))
	repo := huddleRepo(t)
	ctx := context.Background()

	hud, _, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil {
		t.Fatal(err)
	}
	n := time.Now().UnixNano()
	for i, sid := range []string{"a", "b"} {
		if err := repo.Apply(ctx, huddle.Event{
			ID: fmt.Sprintf("ev-j%d-%d", i, n), Type: "participant_joined",
			Room: hud.RoomName, SID: sid, Identity: me,
		}); err != nil {
			t.Fatal(err)
		}
	}

	if err := repo.Apply(ctx, huddle.Event{
		ID: fmt.Sprintf("ev-fin-%d", n), Type: "room_finished", Room: hud.RoomName,
	}); err != nil {
		t.Fatal(err)
	}

	after, err := repo.ByID(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Live() {
		t.Fatal("room_finished did not end the huddle")
	}
	live, err := repo.Participants(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 0 {
		t.Fatalf("%d sessions are still open after the room finished", len(live))
	}
}

// The reconciler repairs a roster after a LOST webhook — which at-least-once
// delivery does not prevent, because it only guarantees a message arrives at
// least once IF it arrives at all.
func TestReconcileMakesTheRosterMatchTheMediaServer(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	other := h.newUser(t, admin, ws, "huddle-peer")
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-reconcile"))
	repo := huddleRepo(t)
	ctx := context.Background()

	hud, _, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = repo.End(context.Background(), hud.ID, "ended_by_user") })

	// Our record says one person is in the call.
	if err := repo.Apply(ctx, huddle.Event{
		ID: fmt.Sprintf("ev-stale-%d", time.Now().UnixNano()), Type: "participant_joined",
		Room: hud.RoomName, SID: "gone", Identity: me,
	}); err != nil {
		t.Fatal(err)
	}

	// The media server says somebody else is, and the first person is not. It
	// is authoritative for presence, so this wins.
	if err := repo.ReconcileParticipants(ctx, hud.ID, []huddle.Participant{{
		HuddleID: hud.ID, ParticipantSID: "present", UserID: other.id,
		JoinedAt: time.Now().Add(-time.Minute), IsScreenSharing: true,
	}}); err != nil {
		t.Fatal(err)
	}

	live, err := repo.Participants(ctx, hud.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("roster has %d live sessions, want 1", len(live))
	}
	if live[0].ParticipantSID != "present" {
		t.Fatalf("roster kept %q; the media server said only \"present\" is in the call",
			live[0].ParticipantSID)
	}
	if !live[0].IsScreenSharing {
		t.Error("the screen-share flag was not taken from the media server")
	}
}

// StaleLive respects the grace period, so a call created a second ago — whose
// first participant has not connected yet — is not ended by the reconciler.
func TestStaleLiveSkipsAFreshHuddle(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-grace"))
	repo := huddleRepo(t)
	ctx := context.Background()

	hud, _, err := repo.StartOrJoin(ctx, ws, huddle.ScopeChannel, channel, me, 10)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = repo.End(context.Background(), hud.ID, "ended_by_user") })

	fresh, err := repo.StaleLive(ctx, time.Minute, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range fresh {
		if x.ID == hud.ID {
			t.Fatal("a huddle created a moment ago is already 'stale'; the reconciler would end " +
				"it before its first participant connected")
		}
	}

	// With no grace it is in the list, which proves the query works at all.
	all, err := repo.StaleLive(ctx, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, x := range all {
		if x.ID == hud.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("the huddle is not listed even with no grace period")
	}
}

// A scope the Go side does not register is a 400, not a row: the capability
// lookup for it does not exist, so accepting it would create a call nothing can
// authorize.
func TestAnUnregisteredScopeIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := huddleRepo(t)

	_, _, err := repo.StartOrJoin(context.Background(), ws, "issue", me, me, 10)
	if err == nil {
		t.Fatal("created a huddle on an unregistered scope, which nothing can authorize")
	}
}

// With no media server configured the routes are ABSENT. A 404 is the truthful
// answer to "start a call" on a deployment that cannot host one; a 500 would
// say something is broken, and it is not.
func TestHuddleRoutesAreAbsentWithoutAMediaServer(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	channel := h.createChannel(t, admin, ws, uniqueSlug("huddle-absent"))

	code, _ := h.do(t, "POST", "/api/v1/channels/"+channel+"/huddle", admin, nil)
	if code != 404 {
		t.Fatalf("POST /huddle = %d on a deployment with no media server, want 404", code)
	}
	// And the webhook, which would otherwise be an unauthenticated writer of
	// the roster.
	code, _ = h.do(t, "POST", "/api/v1/rtc/webhook", "", map[string]any{"id": "x"})
	if code != 404 {
		t.Fatalf("POST /rtc/webhook = %d with no secret configured, want 404", code)
	}
}
