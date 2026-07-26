package ws

import (
	"testing"
	"time"

	natsserver "github.com/nats-io/nats-server/v2/server"
	"github.com/nats-io/nats.go"
)

// testNATS runs an EMBEDDED NATS server for the duration of one test.
//
// Embedded rather than "skip unless NATS_URL is set". A conditional skip is how
// the cross-replica bridge went unverified in the first place: the unit suite
// needs no infrastructure, so a test that quietly opted out of running would
// have looked exactly like a passing one. This costs a few milliseconds and an
// ephemeral port, and it always runs.
func testNATS(t *testing.T) (*nats.Conn, func()) {
	t.Helper()

	srv, err := natsserver.NewServer(&natsserver.Options{
		Host:      "127.0.0.1",
		Port:      -1, // ephemeral, so parallel packages do not collide
		NoLog:     true,
		NoSigs:    true,
		JetStream: false,
	})
	if err != nil {
		t.Fatalf("embedded nats: %v", err)
	}
	go srv.Start()
	if !srv.ReadyForConnections(5 * time.Second) {
		srv.Shutdown()
		t.Fatal("embedded nats did not become ready")
	}

	nc, err := nats.Connect(srv.ClientURL())
	if err != nil {
		srv.Shutdown()
		t.Fatalf("connect to embedded nats: %v", err)
	}
	return nc, func() {
		nc.Close()
		srv.Shutdown()
	}
}
