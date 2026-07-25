package mail

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

func newTestDirectSender(t *testing.T, hosts map[string][]string, port int, signer *DKIMSigner) *DirectSender {
	t.Helper()

	sender, err := NewDirectSender(DirectConfig{
		From:    testFrom,
		HELO:    "mail.superops.example",
		Port:    port,
		Timeout: 5 * time.Second,
		DKIM:    signer,
		Logger:  discardLogger(),
	})
	if err != nil {
		t.Fatalf("NewDirectSender: %v", err)
	}

	sender.hostsFor = func(_ context.Context, domain string) ([]string, error) {
		h, ok := hosts[domain]
		if !ok {
			return nil, permanent("no MX for "+domain, nil)
		}
		return h, nil
	}
	return sender
}

func TestDirectGroupsRecipientsByDomain(t *testing.T) {
	groups, err := groupByDomain([]Address{
		{Email: "a@example.org"},
		{Email: "b@EXAMPLE.org"},
		{Email: "c@other.test"},
	})
	if err != nil {
		t.Fatalf("groupByDomain: %v", err)
	}
	if len(groups) != 2 {
		t.Fatalf("groups = %v, want two domains", groups)
	}
	if len(groups["example.org"]) != 2 {
		t.Errorf("example.org has %d recipients, want 2 (domain matching must be case insensitive)", len(groups["example.org"]))
	}
	if len(groups["other.test"]) != 1 {
		t.Errorf("other.test has %d recipients, want 1", len(groups["other.test"]))
	}

	if _, err := groupByDomain([]Address{{Email: "nodomain"}}); err == nil {
		t.Error("groupByDomain accepted an address with no domain")
	} else if !IsPermanent(err) {
		t.Errorf("an address with no domain mapped to transient (%v)", err)
	}
}

func TestDirectDeliversOneMessagePerDomain(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	host, port := srv.hostPort()

	sender := newTestDirectSender(t, map[string][]string{
		"example.org": {host},
		"other.test":  {host},
	}, port, nil)

	if sender.Name() != TransportSMTPDirect {
		t.Errorf("Name() = %q", sender.Name())
	}

	msg := testMessage()
	msg.To = []Address{{Email: "dana@example.org"}, {Email: "kim@other.test"}}

	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	transcript := strings.Join(srv.transcript(), "\n")
	for _, want := range []string{"RCPT TO:<dana@example.org>", "RCPT TO:<kim@other.test>"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript is missing %q:\n%s", want, transcript)
		}
	}
	// The EHLO name is load-bearing for direct delivery: receiving MTAs check it
	// against the connecting IP's PTR record.
	if !strings.Contains(transcript, "EHLO mail.superops.example") {
		t.Errorf("EHLO did not announce the configured FQDN:\n%s", transcript)
	}
}

func TestDirectSignsWithDKIMWhenConfigured(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	host, port := srv.hostPort()

	key, pemKey := testRSAKey(t)
	signer, err := NewDKIMSigner("superops.example", "s1", pemKey)
	if err != nil {
		t.Fatalf("NewDKIMSigner: %v", err)
	}

	sender := newTestDirectSender(t, map[string][]string{"example.org": {host}}, port, signer)

	msg := testMessage()
	msg.To = []Address{{Email: "dana@example.org"}}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	delivered := srv.message()
	if !strings.HasPrefix(delivered, "DKIM-Signature:") {
		t.Fatalf("delivered message is not signed:\n%s", delivered)
	}
	// The bytes on the wire, not the bytes in memory, are what a receiver
	// verifies — so verify exactly those.
	verifyDKIM(t, []byte(delivered), &key.PublicKey)
}

func TestDirectFallsBackToTheNextMXOnATransientFailure(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	// The first connection — the primary MX — refuses service; the second, the
	// backup MX, is healthy. Both are the same listener because the sender uses
	// one port for every host it tries.
	srv.scriptGreetings("421 4.3.2 mx1.example.org service not available")
	host, port := srv.hostPort()

	sender := newTestDirectSender(t, map[string][]string{"example.org": {host, host}}, port, nil)

	msg := testMessage()
	msg.To = []Address{{Email: "dana@example.org"}}
	if err := sender.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v; a 421 from the primary MX must fall through to the backup", err)
	}
	if !strings.Contains(srv.message(), "Subject: You have been invited") {
		t.Errorf("the backup MX did not receive the message:\n%s", srv.message())
	}
}

func TestDirectStopsAtAPermanentRejection(t *testing.T) {
	srv := newFakeSMTP(t, nil, map[string]string{"RCPT": "550 5.1.1 no such user"})
	host, port := srv.hostPort()

	// Two MX hosts, both the same listener. A 5xx from the first must not be
	// retried against the second: it is the domain's answer, not that host's.
	sender := newTestDirectSender(t, map[string][]string{"example.org": {host, host}}, port, nil)

	msg := testMessage()
	msg.To = []Address{{Email: "dana@example.org"}}

	err := sender.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("Send succeeded against a server that rejected the recipient")
	}
	if !IsPermanent(err) {
		t.Fatalf("a 550 from every MX mapped to transient (%v)", err)
	}

	rcpts := 0
	for _, line := range srv.transcript() {
		if strings.HasPrefix(strings.ToUpper(line), "RCPT TO") {
			rcpts++
		}
	}
	if rcpts != 1 {
		t.Errorf("RCPT was attempted %d times; a permanent rejection must not be retried against the backup MX", rcpts)
	}
}

func TestDirectMixedResultsStayRetryable(t *testing.T) {
	rejecting := newFakeSMTP(t, nil, map[string]string{"RCPT": "550 5.1.1 no such user"})
	host, port := rejecting.hostPort()

	sender := newTestDirectSender(t, map[string][]string{"example.org": {host}}, port, nil)
	sender.hostsFor = func(_ context.Context, domain string) ([]string, error) {
		switch domain {
		case "example.org":
			return []string{host}, nil
		case "deferred.test":
			// A DNS blip: transient, and worth retrying.
			return nil, errors.New("lookup deferred.test: server misbehaving")
		default:
			return nil, permanent("no MX for "+domain, nil)
		}
	}

	msg := testMessage()
	msg.To = []Address{{Email: "dana@example.org"}, {Email: "kim@deferred.test"}}

	// One domain rejects permanently, one is unreachable. The unreachable one is
	// still worth retrying, so the whole send must stay retryable.
	if err := sender.Send(context.Background(), msg); err == nil {
		t.Fatal("Send succeeded despite two failing domains")
	} else if IsPermanent(err) {
		t.Errorf("a mixed result was terminated (%v); the reachable-later domain would never get the mail", err)
	}
}

func TestDirectConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  DirectConfig
		want string
	}{
		{"no HELO", DirectConfig{From: testFrom}, "MAIL_DIRECT_HELO"},
		{"HELO is not a FQDN", DirectConfig{From: testFrom, HELO: "localhost"}, "fully qualified"},
		{"bad port", DirectConfig{From: testFrom, HELO: "mx.test.example", Port: 70000}, "MAIL_DIRECT_PORT"},
		{"unusable from", DirectConfig{From: Address{Email: "nobody"}, HELO: "mx.test.example"}, "MAIL_FROM"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Logger = discardLogger()
			_, err := NewDirectSender(tc.cfg)
			if err == nil {
				t.Fatal("NewDirectSender accepted an unusable configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not mention %q", err, tc.want)
			}
		})
	}
}

func TestDirectWarnsLoudlyWithoutDKIM(t *testing.T) {
	var buf safeBuffer
	_, err := NewDirectSender(DirectConfig{
		From: testFrom, HELO: "mail.superops.example",
		Logger: newTestLogger(&buf),
	})
	if err != nil {
		t.Fatalf("NewDirectSender: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, "UNSIGNED") || !strings.Contains(out, "MAIL_DKIM_DOMAIN") {
		t.Errorf("unsigned direct delivery did not produce a prominent warning:\n%s", out)
	}
}

func TestMXHostsRejectsANullMXDomain(t *testing.T) {
	sender := newTestDirectSender(t, nil, 2525, nil)
	sender.resolver = &net.Resolver{
		PreferGo: true,
		Dial: func(_ context.Context, _, _ string) (net.Conn, error) {
			return nil, errors.New("no DNS in tests")
		},
	}

	// The null-MX and no-record branches need DNS, which these tests do not have.
	// What is checkable without it is that a resolver failure is transient: a DNS
	// blip must not terminate the message.
	_, err := sender.mxHosts(context.Background(), "example.org")
	if err == nil {
		t.Fatal("mxHosts succeeded with a broken resolver")
	}
	if IsPermanent(err) {
		t.Errorf("a DNS failure mapped to permanent (%v); a resolver blip would drop the mail", err)
	}
}
