package mail

import (
	"bufio"
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"log/slog"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// fakeSMTP speaks just enough SMTP to record a conversation and to answer any
// verb with a scripted status line. It is a real net.Listener on loopback, not
// a mock of net/smtp: the point of these tests is the wire behaviour, including
// how net/smtp turns a status line into an error.
type fakeSMTP struct {
	ln net.Listener

	// replies overrides the answer to a verb, e.g. {"RCPT": "550 5.1.1 no such user"}.
	// Anything absent gets the default below.
	replies map[string]string

	// ehloExtensions are advertised after the EHLO greeting line.
	ehloExtensions []string

	// dataReply answers the terminating dot — the acceptance verdict.
	dataReply string

	mu       sync.Mutex
	commands []string
	data     string

	// greetings replaces the 220 banner for the first N connections, so a test
	// can make one MX host fail and the next succeed on a single listener.
	greetings []string
	conns     int
}

func newFakeSMTP(t *testing.T, ext []string, replies map[string]string) *fakeSMTP {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{
		ln:             ln,
		replies:        replies,
		ehloExtensions: ext,
		dataReply:      "250 2.0.0 Ok: queued as ABC123",
	}
	t.Cleanup(func() { _ = ln.Close() })

	go s.serve()
	return s
}

func (s *fakeSMTP) hostPort() (string, int) {
	host, port, _ := net.SplitHostPort(s.ln.Addr().String())
	p, _ := strconv.Atoi(port)
	return host, p
}

func (s *fakeSMTP) serve() {
	for {
		conn, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(conn)
	}
}

func (s *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	_ = conn.SetDeadline(time.Now().Add(10 * time.Second))
	br := bufio.NewReader(conn)

	write := func(format string, args ...any) {
		_, _ = fmt.Fprintf(conn, format+"\r\n", args...)
	}

	greeting := s.nextGreeting()
	write("%s", greeting)
	if !strings.HasPrefix(greeting, "220") {
		return
	}

	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		s.record(line)

		verb := strings.ToUpper(line)
		if i := strings.IndexAny(verb, " :"); i >= 0 {
			verb = verb[:i]
		}

		if reply, ok := s.replies[verb]; ok {
			write("%s", reply)
			if verb == "QUIT" {
				return
			}
			continue
		}

		switch verb {
		case "EHLO":
			if len(s.ehloExtensions) == 0 {
				write("250 mx.test")
				continue
			}
			write("250-mx.test")
			for i, ext := range s.ehloExtensions {
				if i == len(s.ehloExtensions)-1 {
					write("250 %s", ext)
				} else {
					write("250-%s", ext)
				}
			}
		case "HELO":
			write("250 mx.test")
		case "AUTH":
			write("235 2.7.0 Authentication successful")
		case "MAIL", "RCPT", "RSET", "NOOP":
			write("250 2.1.0 Ok")
		case "DATA":
			write("354 End data with <CR><LF>.<CR><LF>")
			var body strings.Builder
			for {
				dl, err := br.ReadString('\n')
				if err != nil {
					return
				}
				if dl == ".\r\n" {
					break
				}
				body.WriteString(dl)
			}
			s.setData(body.String())
			write("%s", s.dataReply)
		case "QUIT":
			write("221 2.0.0 Bye")
			return
		default:
			write("500 5.5.1 unrecognized command")
		}
	}
}

// scriptGreetings replaces the banner for the first len(g) connections.
func (s *fakeSMTP) scriptGreetings(g ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.greetings = g
}

// nextGreeting pops a scripted banner, falling back to a healthy one.
func (s *fakeSMTP) nextGreeting() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	n := s.conns
	s.conns++
	if n < len(s.greetings) {
		return s.greetings[n]
	}
	return "220 mx.test ESMTP fake"
}

// newTestLogger writes structured logs into w, for tests that assert on a
// warning an operator is meant to see.
func newTestLogger(w io.Writer) *slog.Logger {
	return slog.New(slog.NewTextHandler(w, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

func (s *fakeSMTP) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.commands = append(s.commands, line)
}

func (s *fakeSMTP) setData(d string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.data = d
}

func (s *fakeSMTP) transcript() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.commands...)
}

func (s *fakeSMTP) message() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.data
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func newTestSMTPSender(t *testing.T, srv *fakeSMTP, mutate func(*SMTPConfig)) *SMTPSender {
	t.Helper()

	host, port := srv.hostPort()
	cfg := SMTPConfig{
		Host:    host,
		Port:    port,
		TLSMode: TLSNone,
		From:    testFrom,
		Timeout: 5 * time.Second,
		Logger:  discardLogger(),
	}
	if mutate != nil {
		mutate(&cfg)
	}

	sender, err := NewSMTPSender(cfg)
	if err != nil {
		t.Fatalf("NewSMTPSender: %v", err)
	}
	return sender
}

func TestSMTPRunsTheWholeConversation(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	sender := newTestSMTPSender(t, srv, nil)

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if sender.Name() != TransportSMTP {
		t.Errorf("Name() = %q", sender.Name())
	}

	transcript := strings.Join(srv.transcript(), "\n")
	for _, want := range []string{
		"EHLO ",
		"MAIL FROM:<no-reply@superops.example>",
		"RCPT TO:<dana@example.org>",
		"DATA",
		"QUIT",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("transcript is missing %q:\n%s", want, transcript)
		}
	}

	body := srv.message()
	if !strings.Contains(body, "Subject: You have been invited") {
		t.Errorf("delivered message has no subject header:\n%s", body)
	}
	if !strings.Contains(body, "multipart/alternative") {
		t.Errorf("delivered message is not multipart/alternative:\n%s", body)
	}
}

func TestSMTP550IsPermanent(t *testing.T) {
	srv := newFakeSMTP(t, nil, map[string]string{"RCPT": "550 5.1.1 <dana@example.org>: user unknown"})
	sender := newTestSMTPSender(t, srv, nil)

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a server that rejected the recipient")
	}
	if !IsPermanent(err) {
		t.Fatalf("550 mapped to a transient error (%v); the worker would retry a mailbox that does not exist", err)
	}
	if !strings.Contains(err.Error(), "550") {
		t.Errorf("error %q does not carry the server's status code", err)
	}
}

func TestSMTP421IsTransient(t *testing.T) {
	srv := newFakeSMTP(t, nil, map[string]string{"MAIL": "421 4.7.0 too many connections, try later"})
	sender := newTestSMTPSender(t, srv, nil)

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a server that deferred the message")
	}
	if IsPermanent(err) {
		t.Fatalf("421 mapped to a permanent error (%v); greylisting would drop every first attempt", err)
	}
}

func TestSMTPGreylistingAtTheFinalDotIsTransient(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	srv.dataReply = "451 4.7.1 greylisted, try again in 300 seconds"
	sender := newTestSMTPSender(t, srv, nil)

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded against a greylisting server")
	}
	if IsPermanent(err) {
		t.Fatalf("451 at the final dot mapped to permanent (%v)", err)
	}
}

func TestSMTPBadCredentialsArePermanent(t *testing.T) {
	srv := newFakeSMTP(t, []string{"AUTH PLAIN LOGIN"}, map[string]string{
		"AUTH": "535 5.7.8 authentication failed",
	})
	sender := newTestSMTPSender(t, srv, func(cfg *SMTPConfig) {
		cfg.Username = "apikey"
		cfg.Password = "wrong"
		cfg.AllowInsecureAuth = true
	})

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send succeeded with rejected credentials")
	}
	if !IsPermanent(err) {
		t.Fatalf("535 mapped to transient (%v); retrying bad credentials is how an account gets locked out", err)
	}
}

func TestSMTPSendsPlainCredentials(t *testing.T) {
	srv := newFakeSMTP(t, []string{"AUTH PLAIN LOGIN"}, nil)
	sender := newTestSMTPSender(t, srv, func(cfg *SMTPConfig) {
		cfg.Username = "apikey"
		cfg.Password = "s3cret"
		cfg.AllowInsecureAuth = true
	})

	if err := sender.Send(context.Background(), testMessage()); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var authLine string
	for _, line := range srv.transcript() {
		if strings.HasPrefix(strings.ToUpper(line), "AUTH PLAIN ") {
			authLine = line
		}
	}
	if authLine == "" {
		t.Fatalf("no AUTH PLAIN in transcript: %v", srv.transcript())
	}
	decoded, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(authLine, "AUTH PLAIN "))
	if err != nil {
		t.Fatalf("decode AUTH payload: %v", err)
	}
	if string(decoded) != "\x00apikey\x00s3cret" {
		t.Errorf("AUTH payload = %q", decoded)
	}
}

func TestSMTPRefusesCredentialsOverAnUnencryptedConnection(t *testing.T) {
	// Construction refuses first, so an operator learns at boot.
	host, port := newFakeSMTP(t, nil, nil).hostPort()
	_, err := NewSMTPSender(SMTPConfig{
		Host: host, Port: port, TLSMode: TLSNone,
		Username: "apikey", Password: "s3cret",
		From: testFrom, Logger: discardLogger(),
	})
	if err == nil {
		t.Fatal("NewSMTPSender accepted credentials with TLS disabled and no explicit opt-in")
	}
	if !strings.Contains(err.Error(), "MAIL_SMTP_ALLOW_INSECURE_AUTH") {
		t.Errorf("error %q does not name the setting that would allow it", err)
	}

	// And the delivery path refuses independently, so no future caller can
	// construct a target that skips the boot-time check.
	srv := newFakeSMTP(t, []string{"AUTH PLAIN"}, nil)
	h, p := srv.hostPort()
	err = deliverSMTP(context.Background(), smtpTarget{
		addr: net.JoinHostPort(h, strconv.Itoa(p)), serverName: h, helo: "localhost",
		tlsMode: TLSNone, username: "apikey", password: "s3cret",
		allowInsecureAuth: false, logger: discardLogger(),
	}, testFrom.Email, []string{"dana@example.org"}, []byte("Subject: x\r\n\r\nbody\r\n"))

	if err == nil {
		t.Fatal("deliverSMTP sent credentials in the clear")
	}
	if !IsPermanent(err) {
		t.Errorf("refusal is transient (%v); it would be retried forever", err)
	}
	for _, line := range srv.transcript() {
		if strings.HasPrefix(strings.ToUpper(line), "AUTH") {
			t.Fatalf("credentials were sent anyway: %q", line)
		}
	}
}

func TestSMTPRequiresSTARTTLSWhenConfiguredForIt(t *testing.T) {
	// The server advertises no STARTTLS. Configured for starttls, the sender
	// must fail rather than silently downgrade to plaintext.
	srv := newFakeSMTP(t, []string{"8BITMIME"}, nil)
	sender := newTestSMTPSender(t, srv, func(cfg *SMTPConfig) { cfg.TLSMode = TLSStartTLS })

	err := sender.Send(context.Background(), testMessage())
	if err == nil {
		t.Fatal("Send silently downgraded to plaintext")
	}
	if !IsPermanent(err) {
		t.Errorf("error is transient (%v); the server will not grow STARTTLS on a retry", err)
	}
	if !strings.Contains(err.Error(), "STARTTLS") {
		t.Errorf("error %q does not explain what is missing", err)
	}
}

func TestSMTPHonoursContextCancellation(t *testing.T) {
	srv := newFakeSMTP(t, nil, nil)
	sender := newTestSMTPSender(t, srv, nil)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := sender.Send(ctx, testMessage()); err == nil {
		t.Fatal("Send ignored a cancelled context")
	}
}

func TestSMTPConfigValidation(t *testing.T) {
	cases := []struct {
		name string
		cfg  SMTPConfig
		want string
	}{
		{"no host", SMTPConfig{Port: 587, From: testFrom}, "MAIL_SMTP_HOST"},
		{"bad port", SMTPConfig{Host: "relay", Port: 0, From: testFrom}, "MAIL_SMTP_PORT"},
		{"bad tls mode", SMTPConfig{Host: "relay", Port: 587, TLSMode: "ssl", From: testFrom}, "MAIL_SMTP_TLS"},
		{"password without username", SMTPConfig{Host: "relay", Port: 587, Password: "x", From: testFrom}, "MAIL_SMTP_USERNAME"},
		{"unusable from", SMTPConfig{Host: "relay", Port: 587, From: Address{Email: "nobody"}}, "MAIL_FROM"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			tc.cfg.Logger = discardLogger()
			_, err := NewSMTPSender(tc.cfg)
			if err == nil {
				t.Fatal("NewSMTPSender accepted an unusable configuration")
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error %q does not name %s", err, tc.want)
			}
		})
	}
}

func TestChooseAuthPrefersPlainAndFallsBackToLogin(t *testing.T) {
	auth, err := chooseAuth("LOGIN PLAIN CRAM-MD5", "u", "p")
	if err != nil {
		t.Fatalf("chooseAuth: %v", err)
	}
	if _, ok := auth.(plainAuth); !ok {
		t.Errorf("chooseAuth picked %T, want PLAIN when it is offered", auth)
	}

	auth, err = chooseAuth("LOGIN CRAM-MD5", "u", "p")
	if err != nil {
		t.Fatalf("chooseAuth: %v", err)
	}
	if _, ok := auth.(*loginAuth); !ok {
		t.Errorf("chooseAuth picked %T, want LOGIN", auth)
	}

	if _, err := chooseAuth("CRAM-MD5 XOAUTH2", "u", "p"); err == nil {
		t.Error("chooseAuth accepted a server offering no supported mechanism")
	} else if !IsPermanent(err) {
		t.Errorf("unsupported mechanisms mapped to transient (%v)", err)
	}
}
