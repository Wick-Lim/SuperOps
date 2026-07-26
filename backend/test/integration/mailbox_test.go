//go:build integration

package integration

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
	"github.com/Wick-Lim/SuperOps/backend/internal/file"
	"github.com/Wick-Lim/SuperOps/backend/internal/mail"
	"github.com/Wick-Lim/SuperOps/backend/internal/mailbox"
	"github.com/Wick-Lim/SuperOps/backend/pkg/crypto"
)

func mailRepo(t *testing.T) *mailbox.Repository {
	t.Helper()
	h := getHarness(t)
	return mailbox.NewRepository(h.app.DB, h.app.Authz)
}

func newMailbox(t *testing.T, workspaceID, actorID string) *mailbox.Mailbox {
	t.Helper()
	n := time.Now().UnixNano()
	mb, err := mailRepo(t).CreateMailbox(context.Background(), workspaceID,
		fmt.Sprintf("support-%d@example.test", n), "Support", "SUP", actorID)
	if err != nil {
		t.Fatalf("create mailbox: %v", err)
	}
	return mb
}

func inbound(mb *mailbox.Mailbox, suffix string) mailbox.Inbound {
	return mailbox.Inbound{
		ProviderEventID: "ev-" + suffix,
		Recipient:       mb.Address,
		MessageID:       "<msg-" + suffix + "@customer.test>",
		FromAddress:     "customer@customer.test",
		FromName:        "A Customer",
		To:              []string{mb.Address},
		Subject:         "Help with my invoice",
		BodyText:        "Nothing works.",
		RawKey:          "raw/" + suffix + ".eml",
		ReceivedAt:      time.Now(),
	}
}

// THREADING IS BY RFC IDENTIFIER, NEVER BY SUBJECT.
//
// Subject threading merges every customer who ever wrote "Re: invoice" into one
// conversation — a cross-customer data leak dressed up as a feature. Two
// different people with the same subject line must get two conversations, and a
// genuine reply must join the first.
func TestThreadingIsByIdentifierNotBySubject(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	repo := mailRepo(t)
	ctx := context.Background()
	n := time.Now().UnixNano()

	first := inbound(mb, fmt.Sprintf("a-%d", n))
	convA, _, err := repo.Ingest(ctx, first)
	if err != nil {
		t.Fatal(err)
	}

	// A DIFFERENT customer, the SAME subject. Subject threading would file this
	// into the first conversation and show one customer the other's messages.
	other := inbound(mb, fmt.Sprintf("b-%d", n))
	other.FromAddress = "someone-else@elsewhere.test"
	other.Subject = first.Subject
	convB, _, err := repo.Ingest(ctx, other)
	if err != nil {
		t.Fatal(err)
	}
	if convB.ID == convA.ID {
		t.Fatal("two customers with the same subject landed in ONE conversation; each can " +
			"read the other's messages")
	}

	// A genuine reply — In-Reply-To pointing at the first message — joins it.
	reply := inbound(mb, fmt.Sprintf("c-%d", n))
	reply.InReplyTo = first.MessageID
	reply.Subject = "Re: " + first.Subject
	convC, _, err := repo.Ingest(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	if convC.ID != convA.ID {
		t.Fatal("a reply with a matching In-Reply-To started a new conversation")
	}

	// And through References, which is what a long chain actually carries.
	deep := inbound(mb, fmt.Sprintf("d-%d", n))
	deep.References = []string{"<unrelated@x.test>", first.MessageID}
	convD, _, err := repo.Ingest(ctx, deep)
	if err != nil {
		t.Fatal(err)
	}
	if convD.ID != convA.ID {
		t.Fatal("a reply whose References names the thread started a new conversation")
	}
}

// A forged In-Reply-To must not reach another tenant's conversation. The thread
// lookup is scoped to the MAILBOX, which is what makes it impossible.
func TestAForgedInReplyToCannotCrossMailboxes(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	repo := mailRepo(t)
	ctx := context.Background()
	n := time.Now().UnixNano()

	victimBox := newMailbox(t, ws, me)
	victim := inbound(victimBox, fmt.Sprintf("v-%d", n))
	victim.Subject = "Acquisition terms"
	victimConv, _, err := repo.Ingest(ctx, victim)
	if err != nil {
		t.Fatal(err)
	}

	// Another mailbox entirely. The attacker knows — or guesses — the victim's
	// message id and claims to be replying to it.
	attackerBox := newMailbox(t, ws, me)
	forged := inbound(attackerBox, fmt.Sprintf("f-%d", n))
	forged.InReplyTo = victim.MessageID
	forged.FromAddress = "attacker@evil.test"
	forgedConv, _, err := repo.Ingest(ctx, forged)
	if err != nil {
		t.Fatal(err)
	}
	if forgedConv.ID == victimConv.ID {
		t.Fatal("a forged In-Reply-To filed an email into ANOTHER MAILBOX'S conversation; " +
			"the attacker now receives every reply in that thread")
	}
	if forgedConv.MailboxID != attackerBox.ID {
		t.Fatalf("the message was filed into mailbox %s, not %s", forgedConv.MailboxID, attackerBox.ID)
	}
}

// REDELIVERY. Every provider delivers at least once and retries on any non-2xx.
// Without the dedupe key a retry creates a second conversation with the same
// customer and an agent answers one of them.
func TestInboundRedeliveryIsIdempotent(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	repo := mailRepo(t)
	ctx := context.Background()

	in := inbound(mb, fmt.Sprintf("dup-%d", time.Now().UnixNano()))
	if _, _, err := repo.Ingest(ctx, in); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		_, _, err := repo.Ingest(ctx, in)
		if err == nil {
			t.Fatal("a redelivered message was filed again, creating a duplicate conversation")
		}
	}

	var count int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM mail_conversations WHERE mailbox_id = $1`, mb.ID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 1 {
		t.Fatalf("%d conversations after four deliveries of one message", count)
	}
}

// A message with no idempotency key cannot be made safe, so it is refused.
func TestInboundWithoutAnEventIDIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)

	in := inbound(mb, "no-id")
	in.ProviderEventID = ""
	if _, _, err := mailRepo(t).Ingest(context.Background(), in); err == nil {
		t.Fatal("filed a message with no provider event id")
	}
}

// Mail for an address nothing serves is rejected rather than filed somewhere
// plausible. Silently dropping it into the nearest mailbox is how a customer's
// email ends up in front of the wrong team.
func TestMailForAnUnknownAddressIsRejected(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)

	in := inbound(mb, fmt.Sprintf("nomb-%d", time.Now().UnixNano()))
	in.Recipient = fmt.Sprintf("nobody-%d@example.test", time.Now().UnixNano())
	if _, _, err := mailRepo(t).Ingest(context.Background(), in); err == nil {
		t.Fatal("filed mail for an address no mailbox serves")
	}
}

// A customer writing back to a CLOSED conversation re-opens it. This is the
// single most common way a shared inbox loses a request.
func TestAReplyReopensAClosedConversation(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	repo := mailRepo(t)
	ctx := context.Background()
	n := time.Now().UnixNano()

	first := inbound(mb, fmt.Sprintf("close-%d", n))
	conv, _, err := repo.Ingest(ctx, first)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mail_conversations SET state = 'closed' WHERE id = $1`, conv.ID); err != nil {
		t.Fatal(err)
	}

	reply := inbound(mb, fmt.Sprintf("reopen-%d", n))
	reply.InReplyTo = first.MessageID
	again, _, err := repo.Ingest(ctx, reply)
	if err != nil {
		t.Fatal(err)
	}
	if again.ID != conv.ID {
		t.Fatal("the reply did not join the closed thread")
	}
	if again.State != "open" {
		t.Fatalf("state = %q after a customer replied to a closed conversation; the request "+
			"is invisible to everybody working the open queue", again.State)
	}
	if !again.AwaitingReply {
		t.Error("awaiting_reply is false after an inbound message")
	}
}

// The conversation number is a per-mailbox counter, gapless and sequential —
// a gap in a customer-visible reference is indistinguishable from a deletion.
func TestConversationNumbersAreSequentialPerMailbox(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	a := newMailbox(t, ws, me)
	b := newMailbox(t, ws, me)
	repo := mailRepo(t)
	ctx := context.Background()
	n := time.Now().UnixNano()

	for i := 1; i <= 3; i++ {
		conv, _, err := repo.Ingest(ctx, inbound(a, fmt.Sprintf("seq-a%d-%d", i, n)))
		if err != nil {
			t.Fatal(err)
		}
		if conv.Number != int64(i) {
			t.Fatalf("conversation %d has number %d", i, conv.Number)
		}
		if conv.Reference != fmt.Sprintf("SUP-%d", i) {
			t.Fatalf("reference = %q, want SUP-%d", conv.Reference, i)
		}
	}
	// A second mailbox counts from one again: the number is per mailbox, and a
	// global counter would leak how much mail the other teams get.
	conv, _, err := repo.Ingest(ctx, inbound(b, fmt.Sprintf("seq-b-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	if conv.Number != 1 {
		t.Fatalf("a fresh mailbox started at %d", conv.Number)
	}
}

// A CONVERSATION INHERITS ITS MAILBOX'S GRANTS. That is the whole permission
// model for email: an agent is granted once, on the mailbox, and every thread
// in it — past and future — follows.
func TestAConversationInheritsTheMailboxGrant(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	repo := mailRepo(t)
	ctx := context.Background()

	conv, _, err := repo.Ingest(ctx, inbound(mb, fmt.Sprintf("inherit-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}

	agent := h.newGuest(t, admin, ws, "mail-agent")
	convRef := authz.ObjectRef{Type: "conversation", ID: conv.ID}

	// Before the grant: nothing.
	got, err := h.app.Authz.Capability(ctx, authz.UserSubject(agent.id), convRef)
	if err != nil {
		t.Fatal(err)
	}
	if got.Implies(authz.CapRead) {
		t.Fatal("a guest can read a conversation nobody granted them")
	}

	// Grant on the MAILBOX, not on the conversation.
	if err := h.app.Authz.Grant(ctx, authz.UserSubject(me), authz.UserSubject(agent.id),
		authz.ObjectRef{Type: "mailbox", ID: mb.ID}, authz.CapWrite); err != nil {
		t.Fatal(err)
	}

	got, err = h.app.Authz.Capability(ctx, authz.UserSubject(agent.id), convRef)
	if err != nil {
		t.Fatal(err)
	}
	if !got.Implies(authz.CapWrite) {
		t.Fatalf("capability on the conversation = %s after granting write on the mailbox; "+
			"every thread would need its own grant", got)
	}

	// And a conversation that arrives AFTER the grant inherits it too — the
	// property a per-thread grant could not give.
	later, _, err := repo.Ingest(ctx, inbound(mb, fmt.Sprintf("inherit2-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	got, err = h.app.Authz.Capability(ctx, authz.UserSubject(agent.id),
		authz.ObjectRef{Type: "conversation", ID: later.ID})
	if err != nil {
		t.Fatal(err)
	}
	if !got.Implies(authz.CapWrite) {
		t.Fatal("a conversation that arrived after the grant did not inherit it")
	}
}

// Another tenant reaches nothing, over the real HTTP surface.
func TestConversationsAreNotReachableFromAnotherTenant(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)

	conv, _, err := mailRepo(t).Ingest(context.Background(),
		inbound(mb, fmt.Sprintf("tenancy-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}

	stranger := h.newTenant(t, "mail-stranger")
	for _, path := range []string{
		"/api/v1/conversations/" + conv.ID,
		"/api/v1/mailboxes/" + mb.ID + "/conversations",
	} {
		code, _ := h.do(t, http.MethodGet, path, stranger.token, nil)
		if code != http.StatusNotFound {
			t.Errorf("GET %s from another tenant = %d, want 404", path, code)
		}
	}
	code, _ := h.do(t, http.MethodPatch, "/api/v1/conversations/"+conv.ID, stranger.token,
		map[string]string{"state": "spam"})
	if code != http.StatusNotFound {
		t.Errorf("PATCH from another tenant = %d, want 404", code)
	}
}

// An unauthenticated ingest call is refused, and a revoked token stops working.
// This endpoint writes conversations; a stranger reaching it writes into the
// company's inbox.
func TestIngestRequiresALiveToken(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	mb := newMailbox(t, ws, me)
	ctx := context.Background()

	body := map[string]any{
		"event_id":  fmt.Sprintf("ev-http-%d", time.Now().UnixNano()),
		"recipient": mb.Address, "message_id": "<http@customer.test>",
		"from": "customer@customer.test", "subject": "hi", "body_text": "hello",
	}

	code, _ := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", "", body)
	if code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated ingest = %d, want 401", code)
	}
	code, _ = h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", "not-a-real-token", body)
	if code != http.StatusUnauthorized {
		t.Fatalf("ingest with a bogus token = %d, want 401", code)
	}

	// A real token works...
	token := fmt.Sprintf("ingest-%d", time.Now().UnixNano())
	// Hashed in Go, with the same helper the handler uses — a test that hashed
	// it differently would prove the storage shape rather than the lookup.
	if _, err := h.app.DB.Exec(ctx,
		`INSERT INTO mail_ingest_tokens (workspace_id, name, token_hash) VALUES ($1, 'test', $2)`,
		ws, crypto.HashToken(token)); err != nil {
		t.Fatalf("insert ingest token: %v", err)
	}
	code, resp := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", token, body)
	if code != http.StatusCreated {
		t.Fatalf("ingest with a live token = %d (%+v)", code, resp.Error)
	}

	// ...and revoking it stops it, without deleting the row.
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mail_ingest_tokens SET revoked_at = NOW() WHERE workspace_id = $1`, ws); err != nil {
		t.Fatal(err)
	}
	body["event_id"] = fmt.Sprintf("ev-revoked-%d", time.Now().UnixNano())
	code, _ = h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", token, body)
	if code != http.StatusUnauthorized {
		t.Fatalf("ingest with a revoked token = %d, want 401", code)
	}
}

// recordingSender captures what would go on the wire.
type recordingSender struct {
	mu   sync.Mutex
	sent []*mail.Message
}

func (s *recordingSender) Send(_ context.Context, m *mail.Message) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sent = append(s.sent, m)
	return nil
}

func (s *recordingSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sent)
}

// AN AGENT'S REPLY WAS STORED AND NEVER SENT. The handler wrote the row,
// flipped awaiting_reply, returned 201, and the customer received nothing.
// This is the whole delivery path, end to end.
func TestAReplyIsActuallyDeliveredAndThreaded(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()
	n := time.Now().UnixNano()

	// A VERIFIED domain, and a mailbox on it.
	domain := fmt.Sprintf("verified-%d.test", n)
	var domainID string
	if err := h.app.DB.QueryRow(ctx, `
		INSERT INTO mail_domains (workspace_id, domain, verify_token, verified_at)
		VALUES ($1, $2, 'tok', NOW()) RETURNING id::text`, ws, domain).Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	mb, err := mailRepo(t).CreateMailbox(ctx, ws, fmt.Sprintf("support@%s", domain), "Support", "SUP", me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mailboxes SET domain_id = $2 WHERE id = $1`, mb.ID, domainID); err != nil {
		t.Fatal(err)
	}

	inb := inbound(mb, fmt.Sprintf("deliver-%d", n))
	conv, _, err := mailRepo(t).Ingest(ctx, inb)
	if err != nil {
		t.Fatal(err)
	}

	// The agent replies through the real route.
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+conv.ID+"/reply", admin,
		map[string]string{"body_text": "We are on it."})
	var reply struct {
		ID     string  `json:"id"`
		SentAt *string `json:"sent_at"`
	}
	decodeInto(t, resp.Data, &reply)

	// STORED AS UNSENT. A NULL sent_at is what makes the undelivered backlog
	// queryable; stamping NOW() here claimed delivery before anything tried.
	if reply.SentAt != nil {
		t.Fatal("the reply claims it was sent before anything tried to send it")
	}

	// Now deliver it, through the consumer's real path.
	sender := &recordingSender{}
	consumer := mailbox.NewConsumer(h.app.DB, sender, nil)
	if err := consumer.Deliver(ctx, reply.ID); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("the reply was not sent (%d messages); the customer hears nothing", sender.count())
	}

	out := sender.sent[0]
	// FROM THE MAILBOX, not from no-reply@. A reply from a stranger is not a
	// reply.
	if out.From == nil || out.From.Email != mb.Address {
		t.Fatalf("From = %+v, want the mailbox address %s", out.From, mb.Address)
	}
	if len(out.To) != 1 || out.To[0].Email != inb.FromAddress {
		t.Fatalf("To = %+v, want the customer %s", out.To, inb.FromAddress)
	}
	// THREADED. Without In-Reply-To the customer's client shows it as a new
	// conversation, and the agent's careful answer looks unsolicited.
	if out.InReplyTo != inb.MessageID {
		t.Fatalf("In-Reply-To = %q, want the customer's message id %q", out.InReplyTo, inb.MessageID)
	}
	if len(out.References) == 0 {
		t.Error("no References header; a long thread will not stay together")
	}
	if out.MessageID == "" {
		t.Error("no Message-ID; the customer's reply could not be threaded back")
	}

	// STAMPED, so a redelivery does nothing.
	if err := consumer.Deliver(ctx, reply.ID); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("a redelivery sent the reply again (%d total); the customer gets duplicates",
			sender.count())
	}
}

// SENDING FROM AN UNVERIFIED DOMAIN IS REFUSED. Sending as a domain the
// deployment does not control is how its IP gets burned — migration 055's
// header says so, and until now nothing enforced it.
func TestSendingFromAnUnverifiedDomainIsRefused(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()

	// A mailbox with NO verified domain — which is every mailbox created
	// before verification existed.
	mb := newMailbox(t, ws, me)
	conv, _, err := mailRepo(t).Ingest(ctx, inbound(mb, fmt.Sprintf("unver-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+conv.ID+"/reply", admin,
		map[string]string{"body_text": "should not leave the building"})
	var reply struct {
		ID string `json:"id"`
	}
	decodeInto(t, resp.Data, &reply)

	sender := &recordingSender{}
	if err := mailbox.NewConsumer(h.app.DB, sender, nil).Deliver(ctx, reply.ID); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 0 {
		t.Fatal("sent from an unverified domain")
	}

	// And it stays visibly undelivered rather than being marked sent, so it
	// goes out the moment the domain is verified.
	var sentAt *time.Time
	if err := h.app.DB.QueryRow(ctx,
		`SELECT sent_at FROM mail_messages WHERE id = $1`, reply.ID).Scan(&sentAt); err != nil {
		t.Fatal(err)
	}
	if sentAt != nil {
		t.Fatal("a refused message was marked sent")
	}
}

// The sweeper is the backstop for a queue publish that never landed. Without it
// a message-bus hiccup loses an agent's reply permanently.
func TestTheSweeperDeliversWhatTheQueueLost(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()
	n := time.Now().UnixNano()

	domain := fmt.Sprintf("swept-%d.test", n)
	var domainID string
	if err := h.app.DB.QueryRow(ctx, `
		INSERT INTO mail_domains (workspace_id, domain, verify_token, verified_at)
		VALUES ($1, $2, 'tok', NOW()) RETURNING id::text`, ws, domain).Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	mb, err := mailRepo(t).CreateMailbox(ctx, ws, fmt.Sprintf("help@%s", domain), "Help", "HLP", me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(ctx, `UPDATE mailboxes SET domain_id = $2 WHERE id = $1`, mb.ID, domainID); err != nil {
		t.Fatal(err)
	}
	conv, _, err := mailRepo(t).Ingest(ctx, inbound(mb, fmt.Sprintf("sweep-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+conv.ID+"/reply", admin,
		map[string]string{"body_text": "the queue dropped this"})
	var reply struct {
		ID string `json:"id"`
	}
	decodeInto(t, resp.Data, &reply)

	// Age it past the sweeper's grace period, which exists so it never races
	// the consumer for something queued a moment ago.
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mail_messages SET created_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`,
		reply.ID); err != nil {
		t.Fatal(err)
	}

	sender := &recordingSender{}
	n2, err := mailbox.NewConsumer(h.app.DB, sender, nil).SweepUnsent(ctx, 100)
	if err != nil {
		t.Fatal(err)
	}
	if n2 < 1 || sender.count() < 1 {
		t.Fatal("the sweeper delivered nothing; a dropped publish loses the reply permanently")
	}
}

// Ingest and domain administration must be reachable through the product.
// Neither was: no route minted a token, so POST /mail/inbound was a permanent
// 401, and no route created a domain, so nothing could ever be verified.
func TestMailAdministrationIsReachable(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)

	// An ingest token, returned once.
	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/mail/ingest-tokens", admin, map[string]string{"name": "provider"})
	var created struct {
		Token struct {
			ID    string `json:"id"`
			Token string `json:"token"`
		} `json:"token"`
	}
	decodeInto(t, resp.Data, &created)
	if created.Token.Token == "" {
		t.Fatal("the create response carries no plaintext token, so nobody can configure ingest")
	}

	// It works against the real ingest route.
	mb := newMailbox(t, ws, h.whoami(t, admin))
	body := map[string]any{
		"event_id":  fmt.Sprintf("ev-admin-%d", time.Now().UnixNano()),
		"recipient": mb.Address, "message_id": "<admin@customer.test>",
		"from": "customer@customer.test", "subject": "hi", "body_text": "hello",
	}
	if code, r := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", created.Token.Token, body); code != http.StatusCreated {
		t.Fatalf("ingest with a minted token = %d (%+v)", code, r.Error)
	}

	// Listing never returns the secret again.
	var listed []struct {
		ID    string `json:"id"`
		Token string `json:"token"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/workspaces/"+ws+"/mail/ingest-tokens", admin, nil).Data, &listed)
	for _, x := range listed {
		if x.Token != "" {
			t.Fatal("the token list leaks the plaintext secret")
		}
	}

	// Revoking stops it.
	h.req(t, http.StatusNoContent, http.MethodDelete,
		"/api/v1/mail/ingest-tokens/"+created.Token.ID, admin, nil)
	body["event_id"] = fmt.Sprintf("ev-admin2-%d", time.Now().UnixNano())
	if code, _ := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", created.Token.Token, body); code != http.StatusUnauthorized {
		t.Fatalf("a revoked token still works = %d", code)
	}

	// Domains: create returns the DNS instructions, and verification fails
	// until the record exists.
	dresp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/mail/domains", admin,
		map[string]string{"domain": fmt.Sprintf("claim-%d.test", time.Now().UnixNano())})
	var dom struct {
		ID          string `json:"id"`
		VerifyHost  string `json:"verify_host"`
		VerifyValue string `json:"verify_value"`
	}
	decodeInto(t, dresp.Data, &dom)
	if dom.VerifyHost == "" || dom.VerifyValue == "" {
		t.Fatal("the domain response carries no DNS instructions, so it can never be verified")
	}

	var verdict struct {
		Verified bool `json:"verified"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/mail/domains/"+dom.ID+"/verify", admin, nil).Data, &verdict)
	if verdict.Verified {
		t.Fatal("a domain with no TXT record verified; ownership is not being checked")
	}

	// A non-admin may do none of it.
	member := h.newUser(t, admin, ws, "mail-nonadmin")
	if code, _ := h.do(t, http.MethodPost, "/api/v1/workspaces/"+ws+"/mail/ingest-tokens",
		member.token, map[string]string{"name": "x"}); code != http.StatusForbidden {
		t.Fatalf("a non-admin minting an ingest token = %d, want 403", code)
	}
}

// stubResolver is what SetResolver exists for. Verification's SUCCESS path had
// never run: every test could only reach the failure branch, because the real
// resolver was hardcoded and no zone under test publishes our token.
type stubResolver struct{ records map[string][]string }

func (s stubResolver) LookupTXT(_ context.Context, name string) ([]string, error) {
	recs, ok := s.records[name]
	if !ok {
		return nil, fmt.Errorf("NXDOMAIN")
	}
	return recs, nil
}

// VERIFYING A DOMAIN ADOPTS THE MAILBOXES ALREADY ON IT.
//
// Without that an operator verifies the domain, watches mail still not go out,
// and has nothing to look at: the mailbox keeps a NULL domain_id and the
// outbound consumer keeps refusing. It is the difference between a working
// setup flow and one that appears to work.
func TestVerifyingADomainAdoptsItsMailboxesAndUnblocksSending(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()
	n := time.Now().UnixNano()
	domain := fmt.Sprintf("adopt-%d.test", n)

	// The mailbox exists FIRST — the order an operator actually works in, and
	// the order that leaves domain_id NULL.
	mb, err := mailRepo(t).CreateMailbox(ctx, ws, fmt.Sprintf("help@%s", domain), "Help", "ADO", me)
	if err != nil {
		t.Fatal(err)
	}

	resp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/mail/domains", admin, map[string]string{"domain": domain})
	var dom struct {
		ID          string `json:"id"`
		VerifyHost  string `json:"verify_host"`
		VerifyValue string `json:"verify_value"`
	}
	decodeInto(t, resp.Data, &dom)

	// Publish the record, in the resolver the handler will consult.
	h.app.MailboxHandler.SetResolver(stubResolver{
		records: map[string][]string{dom.VerifyHost: {"v=spf1 -all", dom.VerifyValue}},
	})
	t.Cleanup(func() { h.app.MailboxHandler.SetResolver(stubResolver{}) })

	var verdict struct {
		Verified bool   `json:"verified"`
		Reason   string `json:"reason"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodPost,
		"/api/v1/mail/domains/"+dom.ID+"/verify", admin, nil).Data, &verdict)
	if !verdict.Verified {
		t.Fatalf("a published TXT record did not verify: %s", verdict.Reason)
	}

	// THE ADOPTION. The mailbox created before verification now points at the
	// verified domain.
	var adopted *string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT domain_id::text FROM mailboxes WHERE id = $1`, mb.ID).Scan(&adopted); err != nil {
		t.Fatal(err)
	}
	if adopted == nil || *adopted != dom.ID {
		t.Fatal("verifying the domain did not adopt the mailbox already on it; sending stays " +
			"blocked with nothing to look at")
	}

	// And sending now works, which is the property the whole step exists for.
	conv, _, err := mailRepo(t).Ingest(ctx, inbound(mb, fmt.Sprintf("adopt-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	rresp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+conv.ID+"/reply", admin, map[string]string{"body_text": "now it goes"})
	var reply struct {
		ID string `json:"id"`
	}
	decodeInto(t, rresp.Data, &reply)

	sender := &recordingSender{}
	if err := mailbox.NewConsumer(h.app.DB, sender, nil).Deliver(ctx, reply.ID); err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatal("the reply still did not go out after the domain was verified")
	}
}

// THE SWEEP MUST NOT HEAD-OF-LINE BLOCK on messages it will always refuse.
//
// A reply from an unverified domain keeps its NULL sent_at forever. Selecting
// those oldest-first meant a few hundred of them starved every newer reply —
// including ones from a mailbox whose domain IS verified, which the sweeper
// would then never reach.
func TestTheSweepSkipsPermanentlyUnsendableMessages(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()
	n := time.Now().UnixNano()

	// An OLD reply that can never be sent: no verified domain.
	stuck := newMailbox(t, ws, me)
	stuckConv, _, err := mailRepo(t).Ingest(ctx, inbound(stuck, fmt.Sprintf("stuck-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	sresp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+stuckConv.ID+"/reply", admin, map[string]string{"body_text": "stuck"})
	var stuckReply struct {
		ID string `json:"id"`
	}
	decodeInto(t, sresp.Data, &stuckReply)
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mail_messages SET created_at = NOW() - INTERVAL '30 days' WHERE id = $1`,
		stuckReply.ID); err != nil {
		t.Fatal(err)
	}

	// A NEWER reply that CAN be sent.
	domain := fmt.Sprintf("sendable-%d.test", n)
	var domainID string
	if err := h.app.DB.QueryRow(ctx, `
		INSERT INTO mail_domains (workspace_id, domain, verify_token, verified_at)
		VALUES ($1, $2, 'tok', NOW()) RETURNING id::text`, ws, domain).Scan(&domainID); err != nil {
		t.Fatal(err)
	}
	good, err := mailRepo(t).CreateMailbox(ctx, ws, fmt.Sprintf("ok@%s", domain), "OK", "OKM", me)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.app.DB.Exec(ctx, `UPDATE mailboxes SET domain_id = $2 WHERE id = $1`, good.ID, domainID); err != nil {
		t.Fatal(err)
	}
	goodConv, _, err := mailRepo(t).Ingest(ctx, inbound(good, fmt.Sprintf("good-%d", n)))
	if err != nil {
		t.Fatal(err)
	}
	gresp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/conversations/"+goodConv.ID+"/reply", admin, map[string]string{"body_text": "deliverable"})
	var goodReply struct {
		ID string `json:"id"`
	}
	decodeInto(t, gresp.Data, &goodReply)
	if _, err := h.app.DB.Exec(ctx,
		`UPDATE mail_messages SET created_at = NOW() - INTERVAL '10 minutes' WHERE id = $1`,
		goodReply.ID); err != nil {
		t.Fatal(err)
	}

	// A batch of ONE. If the unsendable message were selected it would consume
	// the whole batch and the deliverable one would never be reached.
	sender := &recordingSender{}
	sent, err := mailbox.NewConsumer(h.app.DB, sender, nil).SweepUnsent(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if sender.count() != 1 {
		t.Fatalf("the sweep sent %d messages with a batch of 1; the unsendable message is "+
			"starving the deliverable one", sender.count())
	}
	// AND THE COUNT IS HONEST. Counting a refusal as a delivery made the worker
	// log "delivered replies the queue had lost" on every pass of a deployment
	// whose domain is simply not verified.
	if sent != 1 {
		t.Fatalf("SweepUnsent reported %d delivered, want 1", sent)
	}
}

// AN ATTACHMENT IS A DRIVE FILE, and until now nothing wrote the column that
// makes it one. `files.mail_message_id`, the GC exclusion that spares it and
// the acl_object arm that gives it a path were all protecting a relation
// nothing established.
func TestInboundAttachmentsBecomeDriveFilesAnAgentCanOpen(t *testing.T) {
	h := getHarness(t)
	h.storage(t) // skips when no object storage is configured
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()
	n := time.Now().UnixNano()

	mb := newMailbox(t, ws, me)

	// Mint a token and post through the REAL ingest route, because the
	// attachment path only exists there.
	tresp := h.req(t, http.StatusCreated, http.MethodPost,
		"/api/v1/workspaces/"+ws+"/mail/ingest-tokens", admin, map[string]string{"name": "att"})
	var minted struct {
		Token struct {
			Token string `json:"token"`
		} `json:"token"`
	}
	decodeInto(t, tresp.Data, &minted)

	body := []byte("the quarterly numbers")
	code, resp := h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", minted.Token.Token,
		map[string]any{
			"event_id": fmt.Sprintf("ev-att-%d", n), "recipient": mb.Address,
			"message_id": fmt.Sprintf("<att-%d@customer.test>", n),
			"from":       "customer@customer.test", "subject": "numbers", "body_text": "see attached",
			"attachments": []map[string]any{{
				"filename":     "q3.txt",
				"content_type": "text/plain",
				"content":      base64.StdEncoding.EncodeToString(body),
			}},
		})
	if code != http.StatusCreated {
		t.Fatalf("ingest = %d (%+v)", code, resp.Error)
	}
	var filed struct {
		Conversation struct {
			ID string `json:"id"`
		} `json:"conversation"`
	}
	decodeInto(t, resp.Data, &filed)

	// The thread carries it.
	var thread struct {
		Messages []struct {
			ID          string `json:"id"`
			Attachments []struct {
				FileID string `json:"file_id"`
				Name   string `json:"name"`
			} `json:"attachments"`
		} `json:"messages"`
	}
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet,
		"/api/v1/conversations/"+filed.Conversation.ID, admin, nil).Data, &thread)

	var fileID string
	for _, m := range thread.Messages {
		for _, a := range m.Attachments {
			if a.Name == "q3.txt" {
				fileID = a.FileID
			}
		}
	}
	if fileID == "" {
		t.Fatal("the inbound attachment is not on the message; files.mail_message_id was never written")
	}

	// IT IS A REAL DRIVE OBJECT: openable through the Drive descriptor, with an
	// acl_object row giving it a path.
	var desc driveDescriptor
	decodeInto(t, h.req(t, http.StatusOK, http.MethodGet, "/api/v1/drive/files/"+fileID, admin, nil).Data, &desc)
	if desc.SizeBytes != int64(len(body)) {
		t.Fatalf("size = %d, want %d", desc.SizeBytes, len(body))
	}

	// AND THE COLLECTOR SPARES IT. Without the mail_message_id clause in
	// ListOrphans every attachment is deleted an hour after it arrives.
	repo := file.NewRepository(h.app.DB)
	orphans, err := repo.ListOrphans(ctx, time.Now().Add(time.Hour), 500)
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range orphans {
		if o.ID == fileID {
			t.Fatal("the attachment is an orphan; the collector would delete it within the hour")
		}
	}

	// A filename that would break a header or escape a directory is neutered.
	code, _ = h.doBearer(t, http.MethodPost, "/api/v1/mail/inbound", minted.Token.Token,
		map[string]any{
			"event_id": fmt.Sprintf("ev-evil-%d", n), "recipient": mb.Address,
			"message_id": fmt.Sprintf("<evil-%d@customer.test>", n),
			"from":       "customer@customer.test", "subject": "x", "body_text": "y",
			"attachments": []map[string]any{{
				"filename": "../../etc/passwd\r\nX-Injected: yes",
				"content":  base64.StdEncoding.EncodeToString([]byte("z")),
			}},
		})
	if code != http.StatusCreated {
		t.Fatalf("ingest with a hostile filename = %d", code)
	}
	// What actually matters is that the stored name cannot traverse a path or
	// split a header. Dots that survive once the separators are gone are
	// harmless — "....etcpasswd" is an ugly filename, not an exploit — and
	// asserting on them would be asserting on cosmetics.
	var stored string
	if err := h.app.DB.QueryRow(ctx, `
		SELECT f.name
		  FROM files f
		  JOIN mail_messages m ON m.id = f.mail_message_id
		 WHERE m.message_id = $1`, fmt.Sprintf("<evil-%d@customer.test>", n)).Scan(&stored); err != nil {
		t.Fatalf("the hostile-filename attachment was not stored at all: %v", err)
	}
	if strings.ContainsAny(stored, "/\\\r\n") {
		t.Fatalf("stored filename %q carries a path separator or a line break; it reaches a "+
			"Content-Disposition header and a download path", stored)
	}
	for _, r := range stored {
		if r < 0x20 {
			t.Fatalf("stored filename %q carries a control character", stored)
		}
	}
	if strings.HasPrefix(stored, "..") && !strings.Contains(stored, "etc") {
		t.Fatalf("stored filename %q looks like a traversal attempt", stored)
	}
}

// An agent cannot attach a file they cannot read. A reply is authored by a
// person, and it must not be a way to exfiltrate an object through an email.
func TestAnAgentCannotAttachAFileTheyCannotRead(t *testing.T) {
	h := getHarness(t)
	h.storage(t)
	admin := h.adminToken(t)
	ws := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	ctx := context.Background()

	// A file only the owner can read: a private-channel attachment, the one
	// in-workspace object a fellow member cannot open.
	owner := h.newTenant(t, "att-owner")
	secretID := h.requireFiles(t, owner.token, owner.workspaceID)
	priv := h.createTypedChannel(t, owner.token, owner.workspaceID, uniqueSlug("attpriv"), "private")
	h.req(t, http.StatusCreated, http.MethodPost, "/api/v1/channels/"+priv+"/messages", owner.token,
		map[string]any{"content": "secret", "file_ids": []string{secretID}})

	mb := newMailbox(t, ws, me)
	conv, _, err := mailRepo(t).Ingest(ctx, inbound(mb, fmt.Sprintf("exfil-%d", time.Now().UnixNano())))
	if err != nil {
		t.Fatal(err)
	}

	code, _ := h.do(t, http.MethodPost, "/api/v1/conversations/"+conv.ID+"/reply", admin,
		map[string]any{"body_text": "here you go", "attachment_file_ids": []string{secretID}})
	if code == http.StatusCreated {
		t.Fatal("attached a file the author cannot read; a reply is an exfiltration path")
	}
	if code != http.StatusNotFound {
		t.Fatalf("= %d, want 404 — naming the file would confirm one exists at that id", code)
	}

	var attached int
	if err := h.app.DB.QueryRow(ctx,
		`SELECT count(*) FROM files WHERE id = $1 AND mail_message_id IS NOT NULL`, secretID).Scan(&attached); err != nil {
		t.Fatal(err)
	}
	if attached != 0 {
		t.Fatal("the file was attached despite the refusal")
	}
}

// AN INGEST TOKEN FILES ONLY INTO ITS OWN TENANT.
//
// The tenancy comparison used to live in the handler AFTER Ingest had
// committed and after attachments had been stored, so tenant A's token planted
// a real message — sender, subject, body, files — in tenant B's mailbox and got
// back a 403 describing a write that had already happened.
//
// It also burned the globally unique provider_event_id, so the victim's genuine
// delivery of that event then answered "duplicate" and vanished. That is the
// second half of this test and the reason the fix had to be a lookup scope
// rather than an earlier check: nothing may be written at all.
func TestAnIngestTokenCannotFileIntoAnotherTenant(t *testing.T) {
	h := getHarness(t)
	admin := h.adminToken(t)
	victimWS := h.firstWorkspace(t, admin)
	me := h.whoami(t, admin)
	attacker := h.newTenant(t, "ingest-attacker")
	repo := mailRepo(t)
	ctx := context.Background()

	victimBox := newMailbox(t, victimWS, me)
	n := time.Now().UnixNano()

	plant := inbound(victimBox, fmt.Sprintf("cross-%d", n))
	plant.Subject = "PLANTED BY ANOTHER TENANT"
	plant.WorkspaceID = attacker.workspaceID

	conv, _, err := repo.Ingest(ctx, plant)
	if !errors.Is(err, mailbox.ErrNoMailbox) {
		t.Fatalf("Ingest with a foreign token returned (%v, %v), want ErrNoMailbox", conv, err)
	}

	var landed int
	if err := h.app.DB.QueryRow(ctx, `
		SELECT count(*) FROM mail_messages m
		  JOIN mail_conversations c ON c.id = m.conversation_id
		 WHERE c.mailbox_id = $1 AND m.subject = 'PLANTED BY ANOTHER TENANT'`,
		victimBox.ID).Scan(&landed); err != nil {
		t.Fatal(err)
	}
	if landed != 0 {
		t.Fatalf("%d message(s) reached the victim's mailbox through a foreign ingest token", landed)
	}

	// AND the idempotency key survives, so the victim's real delivery lands.
	// A rolled-back transaction is what makes this true; an early return after
	// the INSERT would not have.
	genuine := inbound(victimBox, fmt.Sprintf("cross-%d", n))
	genuine.Subject = "the customer's actual email"
	genuine.WorkspaceID = victimWS
	if _, _, err := repo.Ingest(ctx, genuine); err != nil {
		t.Fatalf("the victim's genuine delivery was refused after the attempt: %v", err)
	}
}

// A MAILBOX ADDRESS BELONGS TO WHOEVER REGISTERED ITS DOMAIN.
//
// mail_domains has been globally unique from the start — "one tenant per
// domain" — but the constraint only governed SENDING, because 055 reasoned that
// "receiving is harmless". Inbound routing resolves a recipient across every
// tenant by address alone, so it is not: whoever claims an address first
// receives its mail.
//
// The attack is pre-claiming billing@ on a competitor's verified domain. It
// reads their customer email, and because addresses are globally unique it also
// permanently denies the real owner that address.
func TestAMailboxCannotClaimAnotherTenantsDomain(t *testing.T) {
	h := getHarness(t)
	victim := h.newTenant(t, "domain-victim")
	attacker := h.newTenant(t, "domain-attacker")
	repo := mailRepo(t)
	ctx := context.Background()

	// Registered directly: domain creation lives on the admin handler, and the
	// fixture only needs the row that the ownership rule reads.
	domain := fmt.Sprintf("victim-%d.test", time.Now().UnixNano())
	if _, err := h.app.DB.Exec(ctx,
		`INSERT INTO mail_domains (workspace_id, domain, verify_token, verified_at)
		 VALUES ($1, $2, $3, NOW())`,
		victim.workspaceID, domain, "tok-"+domain); err != nil {
		t.Fatalf("register the victim's domain: %v", err)
	}

	_, err := repo.CreateMailbox(ctx, attacker.workspaceID,
		"billing@"+domain, "Billing", "BIL", attacker.id)
	if !errors.Is(err, mailbox.ErrDomainNotYours) {
		t.Fatalf("the attacker created a mailbox on the victim's domain: err = %v", err)
	}

	// The owner may still open one, before verification — registration is the
	// ownership claim; verified_at gates sending, not receiving.
	mb, err := repo.CreateMailbox(ctx, victim.workspaceID,
		"billing@"+domain, "Billing", "BIL", victim.id)
	if err != nil {
		t.Fatalf("the domain's own tenant was refused: %v", err)
	}
	// And the mailbox is bound to the domain row, which nothing but VerifyDomain
	// used to write.
	var boundTo *string
	if err := h.app.DB.QueryRow(ctx,
		`SELECT d.workspace_id::text FROM mailboxes m
		   JOIN mail_domains d ON d.id = m.domain_id WHERE m.id = $1`, mb.ID).Scan(&boundTo); err != nil {
		t.Fatalf("the mailbox was not bound to its domain: %v", err)
	}
	if boundTo == nil || *boundTo != victim.workspaceID {
		t.Fatalf("mailbox domain belongs to %v, want %s", boundTo, victim.workspaceID)
	}

	// An unclaimed domain stays open — a shared demo deployment depends on it.
	if _, err := repo.CreateMailbox(ctx, attacker.workspaceID,
		fmt.Sprintf("support-%d@unclaimed.test", time.Now().UnixNano()),
		"Support", "SUP", attacker.id); err != nil {
		t.Fatalf("an unclaimed domain was refused: %v", err)
	}
}
