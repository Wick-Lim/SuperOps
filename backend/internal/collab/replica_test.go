package collab

import (
	"testing"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/ws"
)

// A collaboration room has to work across replicas, and the two failure modes
// are opposite: relay the wrong way and every update is delivered twice (the
// origin replica delivers it locally AND receives its own NATS publish back),
// or relay not at all and the two halves of a room never see each other. These
// tests use a real NATS server for that reason — an in-process fake would be
// asserting the shape of the code rather than the behaviour of the transport.

func dialNATS(t *testing.T) *nats.Conn {
	t.Helper()
	url := env("NATS_URL", nats.DefaultURL)
	nc, err := nats.Connect(url, nats.Timeout(3*time.Second))
	if err != nil {
		if requireInfra() {
			t.Fatalf("SUPEROPS_REQUIRE_INFRA=1 but NATS at %s is unusable: %v", url, err)
		}
		t.Skipf("NATS unavailable at %s, skipping cross-replica test: %v", url, err)
	}
	t.Cleanup(nc.Close)
	return nc
}

// TestCrossReplicaFanout: two clients on two replicas converge, and each frame
// is delivered exactly once on each side.
func TestCrossReplicaFanout(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)

	f.hub.StartRoomBridge(dialNATS(t), testLogger())

	hub2 := ws.NewHub(testLogger())
	go hub2.Run()
	t.Cleanup(hub2.Shutdown)
	hub2.StartRoomBridge(dialNATS(t), testLogger())
	svc2 := NewService(f.repo, NewWorkspaceAuthorizer(authz.New(f.pool)), hub2, testLogger())

	srv1 := f.serve(t)
	srv2 := serveHub(t, f, hub2, svc2)

	a := srv1.dial(t, f.owner)
	b := srv2.dial(t, f.member)
	join(t, a, doc.ID)
	join(t, b, doc.ID)

	a.send(t, ws.TypeCollabUpdate, map[string]interface{}{
		"document_id": doc.ID,
		"update":      []byte("across"),
	})

	var onOrigin, onRemote updateFrame
	decode(t, a.expect(t, ws.TypeCollabUpdate), &onOrigin)
	decode(t, b.expect(t, ws.TypeCollabUpdate), &onRemote)
	if string(onRemote.Update) != "across" || onRemote.Seq != onOrigin.Seq {
		t.Fatalf("remote replica got %q seq %d, origin got %q seq %d",
			onRemote.Update, onRemote.Seq, onOrigin.Update, onOrigin.Seq)
	}

	// Exactly once on both sides: the origin must not also deliver its own
	// NATS echo, and the remote must not deliver twice.
	a.silent(t, 500*time.Millisecond)
	b.silent(t, 500*time.Millisecond)

	// Awareness crosses the same way and still never touches Postgres.
	b.send(t, ws.TypeCollabAwareness, map[string]interface{}{
		"document_id": doc.ID,
		"state":       []byte("cursor"),
	})
	var aware struct {
		State []byte `json:"state"`
	}
	decode(t, a.expect(t, ws.TypeCollabAwareness), &aware)
	if string(aware.State) != "cursor" {
		t.Fatalf("awareness across replicas = %q, want %q", aware.State, "cursor")
	}
}

// TestCrossReplicaRevocation: the request that withdraws access is served by
// whichever replica the client happens to hit, which is usually not the one
// holding the victim's socket.
func TestCrossReplicaRevocation(t *testing.T) {
	f := newFixture(t)
	doc := f.newDocument(t)

	f.hub.StartRoomBridge(dialNATS(t), testLogger())

	hub2 := ws.NewHub(testLogger())
	go hub2.Run()
	t.Cleanup(hub2.Shutdown)
	hub2.StartRoomBridge(dialNATS(t), testLogger())
	svc2 := NewService(f.repo, NewWorkspaceAuthorizer(authz.New(f.pool)), hub2, testLogger())

	srv2 := serveHub(t, f, hub2, svc2)
	victim := srv2.dial(t, f.member)
	join(t, victim, doc.ID)

	// Revoked on replica 1; the socket is on replica 2.
	f.svc.RevokeAccess(f.member, doc.ID)

	var left struct {
		Reason string `json:"reason"`
	}
	decode(t, victim.expect(t, ws.TypeCollabLeft), &left)
	if left.Reason != "revoked" {
		t.Fatalf("left reason = %q, want revoked", left.Reason)
	}
	if victim.conn == nil {
		t.Fatal("socket disappeared")
	}
}
