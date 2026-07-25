//go:build integration

package integration

import (
	"context"
	"fmt"
	"net/http"
	"testing"
	"time"

	"github.com/Wick-Lim/SuperOps/backend/internal/authz"
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
