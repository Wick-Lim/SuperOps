package presence

import (
	"context"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

func TestParseStatus(t *testing.T) {
	tests := []struct {
		in     string
		want   Status
		wantOK bool
	}{
		{"online", StatusOnline, true},
		{"away", StatusAway, true},
		{"dnd", StatusDND, true},
		{"offline", StatusOffline, true},
		{"", "", false},
		{"Online", "", false},
		{"busy", "", false},
		{`{"xss":"<script>"}`, "", false},
	}

	for _, tt := range tests {
		got, ok := ParseStatus(tt.in)
		if got != tt.want || ok != tt.wantOK {
			t.Errorf("ParseStatus(%q) = (%q, %v), want (%q, %v)", tt.in, got, ok, tt.want, tt.wantOK)
		}
	}
}

// TestTypingExpiryMath pins the sorted-set score arithmetic that replaced the
// per-key TTLs: an entry must survive until exactly typingTTL after it was
// written and be pruned immediately after.
func TestTypingExpiryMath(t *testing.T) {
	base := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	score := typingScore(base)

	tests := []struct {
		name      string
		readAt    time.Duration
		wantPrune bool
	}{
		{"immediately after write", 0, false},
		{"just before expiry", typingTTL - time.Millisecond, false},
		{"exactly at expiry", typingTTL, true},
		{"well after expiry", typingTTL + time.Second, true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cutoff := typingCutoff(base.Add(tt.readAt))
			// ZREMRANGEBYSCORE -inf cutoff removes entries scoring <= cutoff.
			pruned := score <= cutoff
			if pruned != tt.wantPrune {
				t.Errorf("score %d vs cutoff %d: pruned = %v, want %v", score, cutoff, pruned, tt.wantPrune)
			}
		})
	}

	if got := typingScore(base) - typingCutoff(base); got != typingTTL.Milliseconds() {
		t.Errorf("entry lifetime = %dms, want %dms", got, typingTTL.Milliseconds())
	}
}

// fakeStore is a faithful in-memory stand-in for the INCR/DECR/EXPIRE sequences
// the presence Lua scripts run, so the refcount transitions can be exercised
// without a live Redis.
type fakeStore struct {
	conns  map[string]int64
	status map[string]string
}

func newFakeStore() *fakeStore {
	return &fakeStore{conns: map[string]int64{}, status: map[string]string{}}
}

func (f *fakeStore) eval(_ context.Context, script *redis.Script, keys []string, args ...interface{}) (int64, error) {
	connsK, statusK := keys[0], keys[1]
	switch script {
	case connectScript:
		f.conns[connsK]++
		if _, ok := f.status[statusK]; !ok {
			f.status[statusK] = args[1].(string)
		}
		return f.conns[connsK], nil

	case disconnectScript:
		f.conns[connsK]--
		if f.conns[connsK] <= 0 {
			delete(f.conns, connsK)
			delete(f.status, statusK)
			return 1, nil
		}
		return 0, nil

	case heartbeatScript:
		if _, ok := f.status[statusK]; !ok {
			f.status[statusK] = args[1].(string)
		}
		return 1, nil
	}
	t := "unknown script"
	panic(t)
}

func newFakeService() (*Service, *fakeStore) {
	store := newFakeStore()
	return &Service{eval: store.eval}, store
}

func TestPresenceRefcount(t *testing.T) {
	ctx := context.Background()
	svc, store := newFakeService()

	// First device: transition to online.
	conns, err := svc.Connect(ctx, "u1")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conns != 1 {
		t.Fatalf("first Connect returned %d, want 1 (an online transition)", conns)
	}
	if store.status[statusKey("u1")] != string(StatusOnline) {
		t.Fatalf("status = %q, want online", store.status[statusKey("u1")])
	}

	// Second device on the same replica must not evict or re-announce.
	conns, err = svc.Connect(ctx, "u1")
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	if conns != 2 {
		t.Fatalf("second Connect returned %d, want 2", conns)
	}

	// One device disconnecting must NOT mark the user offline.
	offline, err := svc.Disconnect(ctx, "u1")
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if offline {
		t.Fatal("user marked offline while a second device is still connected")
	}
	if _, ok := store.status[statusKey("u1")]; !ok {
		t.Fatal("presence key deleted while a connection is still live")
	}

	// The last one does.
	offline, err = svc.Disconnect(ctx, "u1")
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if !offline {
		t.Fatal("last Disconnect did not report the user offline")
	}
	if _, ok := store.status[statusKey("u1")]; ok {
		t.Fatal("presence key survived the last disconnect")
	}
}

// TestPresenceReconnectRace models the ordering that used to break: the new
// connection registers before the old one's teardown runs.
func TestPresenceReconnectRace(t *testing.T) {
	ctx := context.Background()
	svc, store := newFakeService()

	if _, err := svc.Connect(ctx, "u1"); err != nil { // old socket
		t.Fatalf("Connect: %v", err)
	}
	if _, err := svc.Connect(ctx, "u1"); err != nil { // reconnect
		t.Fatalf("Connect: %v", err)
	}
	offline, err := svc.Disconnect(ctx, "u1") // old socket's deferred teardown
	if err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	if offline {
		t.Fatal("stale teardown marked a freshly reconnected user offline")
	}
	if store.status[statusKey("u1")] != string(StatusOnline) {
		t.Fatal("reconnected user lost their presence key")
	}
}

// TestHeartbeatPreservesChosenStatus: a keepalive must not clobber away/dnd
// back to online.
func TestHeartbeatPreservesChosenStatus(t *testing.T) {
	ctx := context.Background()
	svc, store := newFakeService()

	if _, err := svc.Connect(ctx, "u1"); err != nil {
		t.Fatalf("Connect: %v", err)
	}
	store.status[statusKey("u1")] = string(StatusAway)

	if err := svc.Heartbeat(ctx, "u1"); err != nil {
		t.Fatalf("Heartbeat: %v", err)
	}
	if got := store.status[statusKey("u1")]; got != string(StatusAway) {
		t.Fatalf("status after heartbeat = %q, want away", got)
	}
}

func TestKeyPrefixesDoNotCollide(t *testing.T) {
	// presence-conns: must not be readable as a presence: key, or a refcount
	// would be reported as a user's status.
	if statusKey("u1") == connsKey("u1") {
		t.Fatal("status and refcount keys collide")
	}
	if len(connsKey("u1")) > len(presenceKeyPrefix) && connsKey("u1")[:len(presenceKeyPrefix)] == presenceKeyPrefix {
		t.Fatalf("refcount key %q shares the %q prefix", connsKey("u1"), presenceKeyPrefix)
	}
}
