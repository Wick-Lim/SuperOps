# Plan 08 — Email (shared inbox)

**Phase 6.** Depends on Drive (attachments), on Phase 0's object permissions
(soft — see *Permissions*), and consumes the outbound `internal/mail` work that
is in flight.

Status: design. Not started.

---

## 1. What it is

`support@company.com` arrives as a **conversation** the team works together.
An agent opens the mailbox, sees a list (open / pending / closed, assignee,
snippet, age), opens a conversation, reads the customer's email and every reply
the team has sent, leaves an internal note, assigns it to someone, picks a
canned reply, edits it and sends. The customer sees a normal email thread.

The Front / Help Scout shape, and nothing more than that shape.

A **mailbox is an address bound to a channel**. The channel's roster is the
agent team; channel membership is the only membership question, so
`internal/authz` answers every authorization question in this phase without
growing a parallel model. See §3.

### Explicitly not in scope

- **Personal mailboxes.** ROADMAP §7 cuts them. Nothing here is a step toward
  replacing anyone's Gmail; no IMAP, no POP, no per-user address.
- **Spam filtering.** Consumed as a verdict from upstream, never computed here.
  See §7.
- **An SMTP listener.** v1 receives over HTTP only. §5 explains why, and the
  interface that makes 6b additive.
- SLA timers, business hours, satisfaction surveys, reporting dashboards,
  omnichannel (chat widget / social). This is where Front's surface area lives
  and none of it is load-bearing.
- Rules and auto-assignment. That is Phase 7 triggering on
  `mail.conversation.created` — a seam, not a cut.
- Agent-initiated outbound (emailing a customer who never wrote in). v1 replies
  only; see §11.

---

## 2. Migration number

The tree holds `000`–`012`, plus the in-flight `014_sso` and `015_collab`
(`backend/migrations/`; there is no `013` on disk at all). `016` is reserved for
unified search. **This plan takes `017_email.up.sql` / `.down.sql`**, one file.

It deliberately does **not** add a value to the `channel_type` enum. Migration
009's header notes that golang-migrate wraps each file in a single implicit
transaction and therefore forbids `ALTER TYPE ... ADD VALUE`
(`backend/migrations/009_hardening.up.sql:12-13`). A mailbox points at an
ordinary `private` channel instead, which sidesteps the problem and is the
better model anyway (§3).

One enum *does* need extending: `notification_type`
(`migrations/005_create_notifications.up.sql:1`) gains `'assignment'`. PG 12+
allows `ALTER TYPE ADD VALUE` inside a transaction but forbids *using* the new
value in the same one — the value is added in 017 and first used by Go code in a
later transaction, which is safe. Say so in the migration comment or someone
will "fix" it.

---

## 3. How a mailbox maps onto the existing permission model

**A mailbox owns a `channel_id` pointing at a private channel it does not
otherwise use for messages.** That single foreign key buys:

| Need | Already answered by |
|---|---|
| who are the agents | `channel_members` |
| may this user read this conversation | `authz.CanReadChannel` (`internal/authz/authz.go:214-226`) |
| add / remove an agent | `POST/DELETE /channels/{id}/members` — which already emits `channel.member_added` to the worker's invite notifier (`cmd/worker/main.go:225`) and already revokes live WS subscriptions on removal |
| realtime fan-out to the team | a channel subscription in the existing hub |
| search ACL | the access key `c-<channel_id>` — byte-identical to what `search.MessageDoc` produces (`internal/search/doc.go:289-301`) |

Conversations are **not** rows in `messages`. An email has headers, two bodies, a
direction, recipients and a delivery state; forcing that into
`messages.content` + `metadata` would make `PATCH /messages/{id}` able to edit a
sent email and would put email semantics inside the chat handler. Storage is
separate; the *container* is shared. That is the reuse that matters.

Authorization follows the package's own rule — "never authorize against an id
taken from the URL when the resource names its own parent"
(`internal/authz/authz.go:16-17`). Phase 6 adds exactly **one** authz method,
mirroring `MessageChannel` (`authz.go:232-247`):

```go
// MailboxChannel resolves the channel backing the mailbox that owns a
// conversation. Every conversation-addressed endpoint authorizes through it.
func (c *Checker) ConversationChannel(ctx context.Context, conversationID string) (*ChannelInfo, error)
```

### Against plan 00

When `acl_object` lands, a mailbox becomes an object with path
`/ws:<id>/mailbox:<id>` and conversations inherit from it. Capability mapping:

```
read     see conversations and their contents
comment  leave an internal note
write    reply to the customer, assign, change status
admin    mailbox settings, canned replies, domain config
share    meaningless here — an inbox has no share link. Unused.
```

**Gap I am flagging rather than working around:** `write` on a mailbox means
*emit mail from the company's domain to an arbitrary external recipient*. That is
a different risk class from `write` on a document, and plan 00's ordered ladder
has no way to express it. I am not adding a sixth capability — the ladder's value
is that it is short. Instead: `write` it is, and the compensating controls are
the per-mailbox outbound rate limit (§7), the audit row on every send, and the
fact that reply-all is explicit (§11). If a deployment needs "may read the inbox
but may not send", that is a real request and it needs a decision from whoever
owns plan 00, not a local workaround here.

---

## 4. Data model

### 4.1 New tables

```sql
-- Ownership of a domain, per workspace. Without this, workspace B can claim
-- support@company.com and receive workspace A's mail — the exact class of
-- tenancy bug the integration suite exists because of.
CREATE TABLE mail_domains (
    id                  UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id        UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain              TEXT NOT NULL,                 -- lowercased, IDNA-normalized
    verification_token  TEXT NOT NULL,                 -- DNS TXT the operator publishes
    verified_at         TIMESTAMPTZ,
    inbound_secret_hash TEXT,                          -- sha256, like webhooks.token_hash
    dkim_selector       TEXT NOT NULL DEFAULT '',
    dkim_public_key     TEXT NOT NULL DEFAULT '',      -- what the operator must publish
    created_by          UUID REFERENCES users(id),
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX mail_domains_ws_domain_key ON mail_domains (workspace_id, lower(domain));
CREATE UNIQUE INDEX mail_domains_verified_key  ON mail_domains (lower(domain)) WHERE verified_at IS NOT NULL;

CREATE TABLE mailboxes (
    id             UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id   UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    domain_id      UUID NOT NULL REFERENCES mail_domains(id) ON DELETE RESTRICT,
    channel_id     UUID NOT NULL REFERENCES channels(id) ON DELETE CASCADE,
    address        TEXT NOT NULL,                      -- support@company.com, lowercased
    display_name   TEXT NOT NULL DEFAULT '',
    signature_text TEXT NOT NULL DEFAULT '',
    signature_html TEXT NOT NULL DEFAULT '',
    is_active      BOOLEAN NOT NULL DEFAULT TRUE,
    created_by     UUID REFERENCES users(id),
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mailboxes_channel_key UNIQUE (channel_id)
);
-- Global, not per-workspace: two tenants must never own the same address.
CREATE UNIQUE INDEX mailboxes_address_key ON mailboxes (lower(address));
CREATE INDEX idx_mailboxes_workspace ON mailboxes (workspace_id);

CREATE TABLE mail_conversations (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    mailbox_id      UUID NOT NULL REFERENCES mailboxes(id) ON DELETE CASCADE,
    number          BIGINT NOT NULL,                   -- human-facing, per mailbox
    subject         TEXT NOT NULL DEFAULT '',
    subject_key     TEXT NOT NULL DEFAULT '',          -- normalized: Re:/Fwd: stripped, folded
    status          TEXT NOT NULL DEFAULT 'open'
                    CHECK (status IN ('open','pending','closed','spam')),
    assignee_id     UUID REFERENCES users(id) ON DELETE SET NULL,
    assigned_at     TIMESTAMPTZ,
    participants    TEXT[] NOT NULL DEFAULT '{}',      -- every external address seen, lowercased
    message_count   INT NOT NULL DEFAULT 0,
    last_message_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_inbound_at TIMESTAMPTZ,
    merged_into     UUID REFERENCES mail_conversations(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mail_conversations_number_key UNIQUE (mailbox_id, number)
);
-- The list view, keyset-paginated on (last_message_at, id) exactly as
-- pkg/httputil/pagination.go:22-25 requires a total order.
CREATE INDEX idx_mail_conv_list     ON mail_conversations (mailbox_id, status, last_message_at DESC, id DESC)
                                     WHERE merged_into IS NULL;
CREATE INDEX idx_mail_conv_assignee ON mail_conversations (mailbox_id, assignee_id, status, last_message_at DESC, id DESC)
                                     WHERE merged_into IS NULL;
-- The threading fallback probe (§6, rule 4).
CREATE INDEX idx_mail_conv_thread   ON mail_conversations (mailbox_id, subject_key, last_message_at DESC)
                                     WHERE merged_into IS NULL AND status <> 'closed';
CREATE INDEX idx_mail_conv_parts    ON mail_conversations USING gin (participants);

CREATE TABLE mail_messages (
    id                UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    conversation_id   UUID NOT NULL REFERENCES mail_conversations(id) ON DELETE CASCADE,
    direction         TEXT NOT NULL CHECK (direction IN ('inbound','outbound','note')),

    message_id_header TEXT,                 -- RFC 5322 Message-ID, angle brackets stripped
    in_reply_to       TEXT,
    references_ids    TEXT[] NOT NULL DEFAULT '{}',

    from_name         TEXT NOT NULL DEFAULT '',
    from_address      TEXT NOT NULL DEFAULT '',
    to_addresses      TEXT[] NOT NULL DEFAULT '{}',
    cc_addresses      TEXT[] NOT NULL DEFAULT '{}',
    subject           TEXT NOT NULL DEFAULT '',

    body_text         TEXT NOT NULL DEFAULT '',
    body_html         TEXT NOT NULL DEFAULT '',   -- already sanitized on write
    snippet           TEXT NOT NULL DEFAULT '',   -- quote-stripped, ~200 chars

    author_id         UUID REFERENCES users(id) ON DELETE SET NULL,  -- outbound / note only
    is_auto_reply     BOOLEAN NOT NULL DEFAULT FALSE,
    auth_results      JSONB NOT NULL DEFAULT '{}',  -- SPF/DKIM/DMARC as reported upstream
    spam_score        REAL,

    raw_key           TEXT,                  -- MinIO key of the original RFC822 bytes
    size_bytes        BIGINT NOT NULL DEFAULT 0,
    delivery_status   TEXT CHECK (delivery_status IN ('queued','sent','bounced','failed')),
    delivery_error    TEXT,
    -- Received time, NOT the Date: header. Ordering on a spoofable header is how
    -- a message ends up above the reply that answers it.
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_mail_messages_conv ON mail_messages (conversation_id, created_at, id);
CREATE INDEX idx_mail_messages_msgid ON mail_messages (message_id_header) WHERE message_id_header IS NOT NULL;
CREATE INDEX idx_mail_messages_pending ON mail_messages (delivery_status) WHERE delivery_status = 'queued';

CREATE TABLE mail_attachments (
    mail_message_id UUID NOT NULL REFERENCES mail_messages(id) ON DELETE CASCADE,
    file_id         UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    content_id      TEXT NOT NULL DEFAULT '',   -- for cid: references in HTML
    is_inline       BOOLEAN NOT NULL DEFAULT FALSE,
    PRIMARY KEY (mail_message_id, file_id)
);
CREATE INDEX idx_mail_attachments_file ON mail_attachments (file_id);

-- Ingest staging + idempotency + quarantine. The bytes land here before
-- anything is parsed. See §5.
CREATE TABLE mail_inbound_events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    idempotency_key TEXT NOT NULL,             -- provider event id, else sha256(raw)
    mailbox_id      UUID REFERENCES mailboxes(id) ON DELETE CASCADE,  -- NULL = unroutable
    recipient       TEXT NOT NULL DEFAULT '',
    sender          TEXT NOT NULL DEFAULT '',
    raw_key         TEXT NOT NULL,
    size_bytes      BIGINT NOT NULL,
    status          TEXT NOT NULL DEFAULT 'received'
                    CHECK (status IN ('received','parsed','rejected','quarantined')),
    reject_reason   TEXT NOT NULL DEFAULT '',
    received_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX mail_inbound_idem_key ON mail_inbound_events (idempotency_key);
CREATE INDEX idx_mail_inbound_status ON mail_inbound_events (status, received_at DESC)
       WHERE status <> 'parsed';

CREATE TABLE mail_canned_replies (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    mailbox_id   UUID REFERENCES mailboxes(id) ON DELETE CASCADE,  -- NULL = workspace-wide
    name         TEXT NOT NULL,
    body_text    TEXT NOT NULL DEFAULT '',
    body_html    TEXT NOT NULL DEFAULT '',
    created_by   UUID REFERENCES users(id),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT mail_canned_name_len CHECK (char_length(name) BETWEEN 1 AND 80)
);
CREATE UNIQUE INDEX mail_canned_mailbox_name ON mail_canned_replies (mailbox_id, lower(name)) WHERE mailbox_id IS NOT NULL;
CREATE UNIQUE INDEX mail_canned_ws_name      ON mail_canned_replies (workspace_id, lower(name)) WHERE mailbox_id IS NULL;

-- Lifecycle events, rendered inline in the conversation timeline.
CREATE TABLE mail_conversation_events (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    conversation_id UUID NOT NULL REFERENCES mail_conversations(id) ON DELETE CASCADE,
    actor_id        UUID REFERENCES users(id) ON DELETE SET NULL,
    kind            TEXT NOT NULL,       -- assigned | unassigned | status | merged | split | bounced
    from_value      TEXT NOT NULL DEFAULT '',
    to_value        TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_mail_conv_events ON mail_conversation_events (conversation_id, created_at, id);
```

Length caps on `subject`, `body_text`, `body_html`, `snippet` follow migration
009's `NOT VALID` pattern so they bind new writes without a table scan.

### 4.2 The changes to existing tables — the part that will bite

Attachments go to Drive via `file.Storage`. Nothing new is built for storage.
But an email attachment has no chat `message_id`, and two existing code paths
assume that means "orphan":

```sql
ALTER TABLE files ADD COLUMN mail_message_id UUID REFERENCES mail_messages(id) ON DELETE SET NULL;
ALTER TABLE files ADD CONSTRAINT files_one_owner
    CHECK (message_id IS NULL OR mail_message_id IS NULL) NOT VALID;
CREATE INDEX idx_files_mail_message ON files (mail_message_id) WHERE mail_message_id IS NOT NULL;
```

Three code edits are **mandatory** and each is a live bug if skipped:

1. `file.Repository.ListOrphans` selects `WHERE message_id IS NULL AND created_at < cutoff`
   (`internal/file/repository.go:55-58`). Unchanged, the object GC deletes every
   email attachment 24 hours after it arrives
   (`cmd/worker/main.go:114`, `1131-1157`). It must become
   `message_id IS NULL AND mail_message_id IS NULL`.
2. `file.Handler.canRead` falls back to uploader-only when `message_id` is NULL
   (`internal/file/handler.go:251-264`), so an email attachment would be
   downloadable by nobody. It must resolve `mail_message_id → conversation →
   mailbox → channel` and defer to `CanReadChannel`, which keeps the property
   that authorization is against the container the object actually lives in.
3. `search.FileDoc.Doc()` derives its ACL from `ChannelID` else `UserKey`
   (`internal/search/doc.go:323-339`). Its producer must supply the mailbox's
   channel id for an email attachment, or the attachment is indexed with an ACL
   nobody holds — fail-closed, so it will look like "search is broken" rather
   than a leak, but it is still wrong.

Also: `search.ObjectTypes` is a deliberately closed set (`doc.go:20-37`). Add
`TypeMailConversation ObjectType = "mail_conversation"` and a
`ConversationDoc` whose ACL is `keySet(ChannelKey(mailbox.channel_id))`.

And `ws.Hub.dispatchEvent` switches on a closed list of message types
(`internal/ws/relay.go:45-95`). Mail events need cases there or they are
published to NATS and silently dropped on the way to clients.

---

## 5. Ingest — how mail actually gets in

§3c applies directly: **there is no single correct inbound implementation,
because the right answer is a property of the deployment.** So it is an
interface with a transport chosen by config, validated at boot, defaulting to
off.

```go
// internal/mailin
type Receiver interface {
    // Start begins accepting; it returns when ctx is cancelled.
    Start(ctx context.Context, sink Sink) error
    Name() string
}

// Sink is what every receiver hands raw bytes to. It stages, dedupes and
// publishes — it never parses.
type Sink interface {
    Accept(ctx context.Context, env Envelope, raw io.Reader) (eventID string, err error)
}

type Envelope struct {
    IdempotencyKey string   // provider event id, else sha256 of the raw bytes
    MailFrom       string   // envelope sender (SMTP MAIL FROM), not the From: header
    RcptTo         []string // envelope recipients — the ONLY routing input
    AuthResults    string   // Authentication-Results, if upstream computed it
    SpamVerdict    string
}
```

`MAIL_INBOUND_TRANSPORT` ∈ `off` (default) | `webhook`. Selecting `webhook`
without a verified domain and an inbound secret is a **startup error**, matching
the rule that a mail misconfiguration must fail at boot rather than at first use
(`internal/app/app.go:216-243`).

### v1 ships exactly one inbound contract

```
POST /api/v1/mail/inbound
Content-Type: message/rfc822
X-SuperOps-Signature: t=<unix>,v1=<hex hmac-sha256 of "t.body" with the domain secret>
X-SuperOps-Rcpt: <envelope recipient>
```

No per-provider adapters in Go. Every provider's inbound payload is different
and maintaining four parsers is the connector treadmill ROADMAP §7 already cut
for workflows. Providers that can POST raw MIME hit this endpoint directly;
the rest need a ~30-line shim (a Cloudflare Worker, a Lambda, an nginx `lua`
block) which ships as documentation, not code. **This is a real operator
burden and I am not going to pretend otherwise** — it is the price of not
owning a provider matrix, and it is the right trade for a product whose users
are already running their own Postgres.

The handler is modelled on `webhook.IncomingWebhook`
(`internal/webhook/handler.go:307-314`): no auth middleware, credential in a
header not a path (the legacy path-token form there leaks on every access-log
line — `handler.go:59-64` — do not repeat it), one indistinguishable response
for unknown/disabled/wrong-signature so the endpoint is not an oracle.

### What the SMTP listener would be (6b, not v1)

MX pointed at the deployment, a listener on 25 with STARTTLS. It is the
on-premises answer and the analogue of `mail.TransportSMTPDirect`. Cut from v1
because:

- It is a **public listener anyone on the internet can talk to**, and the
  failure mode of getting it wrong is an open relay, not a bug.
- It needs one new Go dependency. Writing an SMTP server that is correct about
  pipelining, CHUNKING, line-length limits, dot-stuffing and STARTTLS is not a
  place to be original: I would take **`github.com/emersion/go-smtp`** (MIT,
  maintained, small) and nothing else. Named here so 6b does not smuggle it in.
- Shipping both receivers at once is two on-call surfaces in one phase.

The `Receiver` interface above exists so 6b is an additive file, not a refactor.

### MIME parsing

Package `internal/mailin/mimeparse`. Pure function, no infrastructure, unit
tested against a corpus of real `.eml` files.

- **Headers**: `net/mail` + `mime.WordDecoder` with a `CharsetReader` backed by
  `golang.org/x/text/encoding/ianaindex`. The stdlib decoder only knows utf-8,
  us-ascii and iso-8859-1; real mail arrives in windows-1252, Shift_JIS,
  GB2312 and ISO-2022-JP. **`golang.org/x/text` is already in `go.mod` as an
  indirect dependency (v0.35.0)** — promoting it to direct is not a new
  dependency in any meaningful sense.
- **Body selection**: walk the tree. `multipart/alternative` → text/plain into
  `body_text`, text/html into `body_html`. `multipart/related` → the HTML plus
  its `cid:` parts. `multipart/mixed` → bodies plus attachments.
  `multipart/signed` → the first part is the content. `message/rfc822` → an
  attachment; **do not recurse into it as body**, or a forwarded thread becomes
  the conversation's body.
- **Budgets, enforced, every one of them a `PermanentError`**: depth ≤ 20,
  parts ≤ 200, decoded bytes ≤ 25 MiB (`MAIL_MAX_MESSAGE_BYTES`), headers ≤ 200,
  wall clock under the worker's `handlerTimeout` (`cmd/worker/main.go:96`).
  A permanent error terminates the event with a reason instead of redelivering
  it five times (`cmd/worker/main.go:579-586`), and the event row goes to
  `quarantined` where an admin can see it.
- **HTML sanitization**: emails carry arbitrary HTML and the SPA shares an
  origin with the API — the file handler already documents that this makes
  anything scriptable a stored-XSS plus token-exfiltration vector
  (`internal/file/handler.go:56-59`). Two layers:
  1. Server-side allowlist on write. This needs
     **`github.com/microcosm-cc/bluemonday`** (BSD-3) — the second and last new
     dependency, and justified on the same grounds as the SMTP server: a
     hand-rolled HTML sanitizer is a CVE with a release schedule.
  2. Client renders `body_html` in `<iframe sandbox srcdoc>` with
     `img-src 'self'`, so remote images (tracking pixels) are blocked by default
     with a per-conversation opt-in that is not remembered.
  `cid:` references are rewritten to `/api/v1/files/{id}` before storage.
  If §14 Q2 comes back "text only is fine", bluemonday is not needed.
- **Quote stripping** is heuristic and only ever affects `snippet` and a
  client-side collapse toggle. The full body is always stored. A stripper that
  is wrong about `On …, … wrote:` in Finnish costs a slightly worse preview,
  which is the correct blast radius for a heuristic.

---

## 6. Threading

Resolution order, first match wins:

1. **Our own reference.** Every outbound message gets a Message-ID we mint:
   `<c.{conversation_id}.{rand}@{mailbox domain}>`. On inbound, walk
   `In-Reply-To` then `References` right-to-left for an id in our domain whose
   local part resolves to a conversation in *this* mailbox. Exact, and immune to
   subject mangling. This is the primary mechanism and it works for every client
   that echoes References at all.
2. **Stored Message-ID.** Any id in `In-Reply-To`/`References` matched against
   `mail_messages.message_id_header`. Catches replies to mail we did not
   originate (a customer forwarding a colleague's email into the thread).
3. **Reply-To token.** `Reply-To: support+c<hmac-token>@domain` on outbound, and
   plus-address stripping on inbound lookup. Belt and braces for the clients
   that drop References entirely. The token is HMAC'd, never a bare uuid.
4. **Heuristic fallback**, and only under all four conditions: same mailbox,
   `subject_key` equal, the sender is **already in `participants`**, and the
   candidate's `last_message_at` is within 7 days and its status is not
   `closed`.

Otherwise: a new conversation.

`subject_key` normalization: repeatedly strip a leading reply/forward prefix
(`Re:`, `RE[2]:`, `Fwd:`, `FW:`, `Aw:`, `Antwort:`, `Rép:`, `回复:`, `转发:`,
`Sv:`, `Vs:`), collapse whitespace, lowercase, cap length. Table-tested.

### When clients get it wrong

They do, constantly. The governing principle:

> **Over-merging is a data leak; under-merging is a click.**

Two customers' threads merged into one conversation means an agent replies to
the wrong person and quotes the other customer's text back at them. That is a
disclosure incident. A conversation that should have merged and did not is an
agent pressing "merge". So every heuristic leans away from merging — rule 4's
participant requirement is exactly what stops twenty different customers who all
wrote "Invoice" to `support@` from landing in one conversation, which is the
canonical subject-threading disaster.

And because the heuristic is deliberately conservative, it needs an escape
hatch, so **merge and split ship in v1**:

- `POST /api/v1/conversations/{id}/merge {source_conversation_id}` — moves the
  source's messages, unions `participants`, sets `merged_into`, writes a
  `merged` timeline event. Reversible via split.
- `POST /api/v1/conversations/{id}/split {from_message_id}` — moves that message
  and everything after it into a new conversation. Cheap: the timeline is rows
  with a `conversation_id`.

Both are one `UPDATE ... SET conversation_id` in a transaction that also
recomputes `message_count`, `last_message_at` and `participants`. Without them
the threading heuristic has no recovery path and the first bad merge is
permanent.

### Auto-replies and loops

Detect `Auto-Submitted: auto-*`, `Precedence: bulk|list|junk`, `List-Id`,
`X-Autoreply`, and DSN content types. A detected auto-reply sets
`is_auto_reply`, **does not reopen a closed conversation**, and **does not
notify**. An out-of-office replying to our reply replying to the out-of-office
is how a mailbox generates ten thousand messages overnight, and this is the
whole defence. Note the outbound side already sets `Auto-Submitted:
auto-generated` on transactional mail (`internal/mail/mime.go:126-128`) — agent
replies must **not** carry it; they are human mail and the header would cause
some receivers to suppress them.

### Bounces

VERP: the envelope sender on an agent reply is
`bounces+<hmac(mail_message_id)>@domain`. A DSN then names the exact outbound
message in its Return-Path, with no DSN body parsing at all. Ingest checks the
bounce prefix **before** mailbox lookup, sets `delivery_status='bounced'` and
writes a `bounced` timeline event, so the agent sees "this reply bounced"
instead of silence. Cheapest high-value deliverability feature in the phase.

---

## 7. Deliverability, spam, abuse — what code can and cannot assert

**Deliverability is not a coding problem.** SPF/DKIM/DMARC alignment, IP and
domain reputation, warmup — none of it is something this codebase can assert,
and a plan that implies otherwise is lying. What the product *can* do:

- **An admin DNS check.** `GET /api/v1/workspaces/{id}/mail/domains/{id}/dns`
  resolves the live SPF TXT, the DKIM selector TXT, DMARC and MX for the domain
  and reports present-vs-required with the exact records to publish. This is
  §3c rule 3 ("give the operator a way to verify") applied to inbound, and it is
  the difference between "mail works" and "mail silently goes to spam".
- **Refuse to activate a mailbox on an unverified domain.** Boot-time and
  create-time, not first-send.
- **Surface bounce and complaint rate per mailbox** in `/metrics`
  (`internal/app/metrics.go` already exists).

**Spam filtering is not implemented here and will not be.** It is its own
discipline with its own release cadence. Three honest positions:

1. Webhook ingest: the provider already scored it. Store `spam_score` /
   verdict from the shim's headers; a spam verdict routes to `status='spam'`,
   never to the open queue.
2. SMTP ingest (6b): the deployment puts rspamd or SpamAssassin in front. Ship
   a compose profile, not Go code.
3. Always: **never** trust the `From:` header for routing or for "is this the
   customer". Routing is on the envelope recipient; identity is
   `Authentication-Results` if present and "unverified" otherwise.

Abuse controls, all reusing `internal/ratelimit` (Redis-backed, already wired
for the mail-test endpoint at `internal/app/app.go:352-362`): per-remote-IP and
per-mailbox limits on ingest, returning **429** (which providers correctly treat
as retry-later) rather than accepting unboundedly; a per-mailbox daily outbound
cap with an admin alert.

---

## 8. API surface

Conventions unchanged: `RegisterRoutes(mux, authMw)`, `{data,meta,error}` via
`httputil.JSON` / `JSONList`, keyset cursors via `EncodeCursor(t, id)`
(`pkg/httputil/pagination.go:64`), `err != nil` → 500 and `!ok` → 403/404, never
collapsed.

```
# Admin (adminMw = authMw + RequireAnyWorkspaceAdmin, per app.go:344-347)
POST   /api/v1/workspaces/{workspace_id}/mail/domains
GET    /api/v1/workspaces/{workspace_id}/mail/domains
POST   /api/v1/workspaces/{workspace_id}/mail/domains/{domain_id}/verify
GET    /api/v1/workspaces/{workspace_id}/mail/domains/{domain_id}/dns
PUT    /api/v1/workspaces/{workspace_id}/mail/domains/{domain_id}/secret   # rotate
DELETE /api/v1/workspaces/{workspace_id}/mail/domains/{domain_id}

POST   /api/v1/workspaces/{workspace_id}/mailboxes
GET    /api/v1/workspaces/{workspace_id}/mailboxes
PATCH  /api/v1/workspaces/{workspace_id}/mailboxes/{mailbox_id}
DELETE /api/v1/workspaces/{workspace_id}/mailboxes/{mailbox_id}
GET    /api/v1/workspaces/{workspace_id}/mail/inbound-events?status=quarantined

# Agent surface (authMw; authorized by the mailbox's channel)
GET    /api/v1/mailboxes/{mailbox_id}/conversations?status=&assignee=&cursor=&limit=
GET    /api/v1/conversations/{conversation_id}
GET    /api/v1/conversations/{conversation_id}/messages?cursor=&limit=
PATCH  /api/v1/conversations/{conversation_id}            # status, subject, assignee_id
POST   /api/v1/conversations/{conversation_id}/claim      # atomic assign-if-unassigned
POST   /api/v1/conversations/{conversation_id}/reply
POST   /api/v1/conversations/{conversation_id}/note
POST   /api/v1/conversations/{conversation_id}/merge      {source_conversation_id}
POST   /api/v1/conversations/{conversation_id}/split      {from_message_id}
GET    /api/v1/conversations/{conversation_id}/messages/{mail_message_id}/raw

GET    /api/v1/workspaces/{workspace_id}/mail/canned-replies
POST   /api/v1/workspaces/{workspace_id}/mail/canned-replies
PATCH  /api/v1/workspaces/{workspace_id}/mail/canned-replies/{id}
DELETE /api/v1/workspaces/{workspace_id}/mail/canned-replies/{id}

# Ingest — no auth middleware, HMAC-authenticated
POST   /api/v1/mail/inbound
```

Two routing notes taken from scars already in the tree:

- Everything lives under `/mail/`, `/mailboxes/` and `/conversations/` so the
  wildcard patterns stay disjoint. `webhook/handler.go:48-53` documents a
  `ServeMux` **registration panic** from two patterns where neither is more
  specific; the same hazard applies to any route ending in a literal segment
  under a `{id}` wildcard.
- Message-addressed routes hang off `/conversations/{id}/...` rather than a
  top-level `/messages/{id}` — that namespace already belongs to chat
  (`internal/message/handler.go:63-65`), both ids are uuids, and a caller who
  mixes them up should get a clean 404 from one handler rather than a confusing
  one from the other.

**The reply contract**, because it carries the collision guarantee:

```jsonc
POST /api/v1/conversations/{id}/reply
{
  "body_text": "...",
  "body_html": "...",              // optional, sanitized server-side
  "cc": ["..."],                    // explicit; reply-all is never the default
  "attachment_file_ids": ["..."],
  "expected_last_message_id": "..." // optimistic concurrency — see §9
}
→ 201 {data: <mail_message>}
→ 409 {error:{code:"CONVERSATION_CHANGED"}, data:{missed:[<mail_message>...]}}
```

---

## 9. Assignment and collision detection

Two agents replying at once is the defining failure of a shared inbox, and it
needs both a soft signal and a hard guarantee. Only one of them actually works.

**Soft — presence.** Opening a conversation broadcasts an ephemeral
`mail.viewing` / `mail.composing` frame over the existing hub, on the mailbox's
channel subscription, relayed cross-replica by the NATS bridge exactly as
`typing.indicator` already is (`internal/ws/bridge.go:36-60`). Cost: one new
frame type in `protocol.go` and one case in `dispatchEvent`. Zero storage,
zero new infrastructure. It shows "Jane is replying" and it will lose races.

**Hard — optimistic concurrency on the reply.** `expected_last_message_id` is
checked inside the transaction that appends:

```sql
SELECT id, message_count FROM mail_conversations WHERE id = $1 FOR UPDATE;
-- compare the newest mail_messages.id against expected_last_message_id
```

Mismatch → **409 `CONVERSATION_CHANGED`** with the messages the client has not
seen, so the UI says "Jane replied while you were typing" and the agent decides.
This is the ~15 lines that actually prevent a double reply, and it is the reason
the soft signal is allowed to be best-effort.

**Assignment is its own race.** "Take it" must be atomic:

```sql
UPDATE mail_conversations
   SET assignee_id = $2, assigned_at = NOW()
 WHERE id = $1 AND assignee_id IS NULL
RETURNING id;
```

Zero rows → 409 with the winner's name. `PATCH` with an explicit `assignee_id`
is the reassignment path and is unconditional (an admin overriding). Both write
a `mail_conversation_events` row, a `notification` of the new `'assignment'`
type for the assignee, and a `mail.conversation.assigned` event on
`superops.{workspace}.mail.conversation.assigned` so every agent's list updates
live.

---

## 10. Package layout

| Package | Owns | Reuses rather than rebuilds |
|---|---|---|
| `internal/mailbox` | mailboxes, conversations, messages, assignment, notes, canned replies, merge/split. Handler + repository. | `authz` (one new method), `httputil`, `notification`, `audit`, `ws` hub |
| `internal/mailbox/thread.go` | threading resolution as pure functions over a header set plus a lookup interface | — |
| `internal/mailin` | the HTTP receiver, HMAC verification, raw staging to MinIO, idempotency, the JetStream publish, and the durable consumer that parses and threads | `file.Storage`, `pkg/nats`, `ratelimit`, `cmd/worker`'s `bindDurable` |
| `internal/mailin/mimeparse` | MIME → `Parsed{headers, bodies, attachments}`. No DB, no network, no infra. | stdlib + `x/text` |
| `internal/mail` | **extended, not forked** — see below | itself |

Unchanged and simply used: `internal/authz`, `internal/file`, `internal/search`,
`internal/notification`, `internal/ws`, `internal/ratelimit`, `internal/audit`,
`pkg/httputil`, `pkg/crypto` (`HashToken` for the inbound secret, same as
webhooks), `pkg/database`.

### The `internal/mail` extension, and one decision it contradicts

`mail.Message` (`internal/mail/mail.go:67-81`) has no `From`, no threading
headers, no attachments and no envelope sender. An agent reply needs all four.
So:

1. `Message.From *Address` (nil = transport default), typed `InReplyTo string`
   and `References []string` — **not** a free-form header bag, which would
   reintroduce exactly the header-injection vector `checkAddress` and
   `Validate` exist to close (`mail.go:100-105`, `mime.go:216-229`).
2. `Message.EnvelopeFrom string` for VERP.
3. `Message.Attachments []Attachment{Filename, ContentType, ContentID, Inline, Body}`,
   and `buildBody` (`mime.go:148-173`) extended from `multipart/alternative` to
   the full `mixed(related(alternative(text,html), inline…), attachments…)`
   nest. Fiddly, and `mime_test.go` is already set up to test exactly this.
4. **A second queue kind.** `mail.Request` carries a fully-rendered `Message`
   on purpose, so "the worker then needs no database access to send"
   (`queue.go:34-50`). That reasoning does not transfer, for two reasons: an
   invitation's inputs can be revoked between queue and send while a
   `mail_messages` row is immutable once written; and **JetStream's default max
   payload is 1 MiB**, so a reply with a 10 MB attachment cannot ride that shape
   at all. Phase 6 adds `mail.outbound.requested` carrying
   `{workspace_id, mail_message_id}`, a second durable consumer
   (`mailer-conversation`) on the same stream, and reuses the same `Sender`.
   I am contradicting an in-flight decision here deliberately and narrowly —
   the invitation path keeps its rendered payload.

New durable consumers in `cmd/worker`, both via the existing `bindDurable`:

```
mail-ingest           superops.*.mail.inbound.received   → parse, thread, persist
mailer-conversation   superops.*.mail.outbound.requested → render from row, send
```

---

## 11. The hard part

**Ingest is a security and availability boundary whose input you do not
control, and the failure that will actually page you is silence.**

Everything upstream of the parser is chosen by anyone on the internet who knows
the address, delivered by a system that retries, into code that must never
crash, never allocate unboundedly, never emit HTML that executes, and never
merge two customers into one thread. And it must answer 200 quickly, or the
provider queues, retries and eventually bounces mail the customer believes was
delivered.

How I would attack it, in order:

**1. Split accept from parse.** The HTTP handler does four things: verify the
HMAC, cap the body, `PutObject` the raw bytes, insert `mail_inbound_events` on
the unique idempotency key, publish. It parses *nothing* and returns in
single-digit milliseconds. Consequences worth stating plainly:

- A parser bug cannot produce a 500 that makes a provider redeliver forever.
- Every message ever received is on disk in its original form, so a parser fix
  is **retroactive** — re-publish the event and reparse — instead of a
  data-loss event.
- Idempotency is a unique index, not a code path.

This is the publish/consume split `internal/mail` already has, run backwards.

**2. The parser is a pure function with hard budgets** (§5), and every budget
violation is a `PermanentError` so the worker terminates with a reason
(`cmd/worker/main.go:579-586`) into `status='quarantined'`, visible to an admin,
rather than burning five redeliveries. Fuzz the part walker
(`go test -fuzz=FuzzParse`) seeded with the corpus; a mail bomb is 200 lines of
Python and someone will send one.

**3. Threading biased against merging**, with merge/split as the documented
recovery path (§6). The asymmetry is the whole design: a wrong merge is a
cross-customer disclosure, a missed merge is a click.

**4. Rendering sandboxed twice** — allowlist on write, `iframe sandbox` +
`img-src 'self'` on read, `cid:` rewritten same-origin. The existing file
handler's refusal to serve `text/html` inline (`internal/file/handler.go:60-74`)
covers attachments for free; do not weaken it for "view original", which
downloads.

**5. Backpressure that degrades correctly.** Over the per-IP or per-mailbox
limit, return 429 — providers retry on 429 and drop on 400. Accepting
unboundedly during a newsletter blast or a mail loop is how the bucket and the
worker queue both go.

**6. Instrument silence.** The parser will be fine after a month. What will not
be fine is `support@` receiving nothing for four days because a DNS record
changed, a provider suspended the account, or one side rotated the HMAC secret.
There is no natural alarm for "no mail arrived" — a quiet inbox looks identical
to a broken one. So: a `mail_inbound_last_received_seconds{mailbox}` gauge in
`/metrics`, and "last received: 4 days ago" on the mailbox admin screen. This is
the cheapest item in the phase and the one most likely to matter at 3am.

---

## 12. Sequencing

Start **the parser** and **the schema** on day one; they share nothing and
everything downstream is shaped by what the parser produces.

| # | Work | Size | Depends on | Parallel with |
|---|---|---|---|---|
| 1 | 017 schema + `internal/mailbox` repository + conversation read endpoints | S | — | 2, 3, 7 |
| 2 | Ingest accept path (HMAC, staging, idempotency, publish) | S | 1 | 3 |
| 3 | **MIME parser + corpus + fuzz** | M | — | everything |
| 4 | Threading + conversation creation + merge/split | M | 1, 3 | 5 |
| 5 | `internal/mail` extension + second queue kind + `mailer-conversation` | M | 1 | 4 |
| 6 | Assignment, status, collision, notes, canned replies | M | 4 | 7 |
| 7 | **Client**: list / conversation / composer / assignment | L | 1 | 3–6 |
| 8 | Admin: domains, verification, DNS check, secret rotation, quarantine view | M | 1 | 6 |
| 9 | Search integration + retention/GC corrections (§4.2) | S | 4 | — |

**Long pole: the client (7).** It is a genuinely new surface — a three-pane
list/thread/composer — not a variation on the chat pane, though it inherits the
responsive shell from `d0a484b`. On the backend the long pole is 3→4: parser
correctness and threading correctness are where the time actually goes, and
neither can be rushed by adding people.

Ships first as something usable: 1 + 2 + 3 + 4 is a working read-only shared
inbox. Replying (5) makes it a product.

---

## 13. Risks and failure modes

- **Silent inbound outage.** §11.6. The one to instrument before anything else.
- **Cross-customer merge.** Mitigated by the participant requirement; the
  residual risk is real and is why merge/split and a timeline event exist.
- **The raw-bytes bucket grows forever.** Every message keeps its RFC822
  original. 50k messages/month × 100 KB is 5 GB/month with no collector.
  `workspaces.retention_days` exists (migration 007) and `runRetention`
  (`cmd/worker/main.go:902-962`) only knows about `messages` — it must learn
  about mail, or this is a permanent leak. Paired with the `ListOrphans` bug in
  §4.2, which deletes email attachments 24 hours after arrival if not fixed.
  These two are the highest-value cross-package edits in the plan.
- **JetStream's 1 MiB payload limit** vs. attachments — solved by putting the
  row id on the queue (§10). If anyone later "simplifies" that by inlining raw
  MIME, it breaks on exactly the messages that matter most.
- **Reply-all footgun.** An agent hits reply-all on a thread that CCs a mailing
  list and emails hundreds of people from the company domain. Default is reply
  to the last inbound sender only; reply-all is an explicit action that shows
  the recipient count before sending.
- **Address reuse across tenancy.** Global unique index plus domain
  verification. If a mailbox is deleted and another workspace claims the
  address, in-flight replies route to the new owner — hold deleted addresses as
  tombstones for N days.
- **`mail_conversations` row contention.** Reply ordering takes the conversation
  row lock. Fine at support volume; it would not be at newsletter volume, which
  is another reason ingest is rate-limited.
- **Search relevance.** Index the quote-stripped body, not the full one, or
  every conversation matches every other conversation via the quoted history.
- **`Date:` headers are frequently absent or wrong.** Order on received time,
  display the header. Already encoded in the schema comment.
- **EAI / SMTPUTF8 addresses** (`用户@例子.广告`). Rejecting them in v1 is
  defensible; silently mangling them is not. Store as received, IDNA-normalize
  for domain comparison only.
- **Bounce storms.** A bad address on a large thread produces one DSN per
  recipient. They are inbound mail and are subject to the same rate limits;
  they must not create conversations (§6).

---

## 14. Verification

### Unit — no infrastructure, runs in the ordinary `go test ./...` lane

- `internal/mailin/mimeparse` against `testdata/*.eml`: Gmail, Outlook/Exchange
  (`multipart/related` + windows-1252 HTML), Apple Mail, a plain-text mailer, a
  nested `message/rfc822` forward, base64 with mangled CRLF, an ISO-2022-JP
  subject, a message with no `Message-ID`, a DSN, an out-of-office, a 200-part
  bomb and a 30-deep nesting bomb. Assert bodies, attachments, and that both
  bombs return a `PermanentError`.
- `FuzzParse` over the part walker.
- Threading as a table test: correct reply; subject-match from a **stranger**
  (must NOT merge); subject-match from a participant (must merge); a client that
  drops References; `Re:` in five languages; a 40-hop References chain; a
  message that references a conversation in a different mailbox (must NOT
  merge).
- `internal/mail`: `mime_test.go` extended for `multipart/mixed`, inline `cid:`,
  `In-Reply-To`/`References` folding, and per-message `From`/`EnvelopeFrom`.

### Integration — `test/integration`, `-tags=integration`, real infra

The suite runs **without a worker** and the search tests already index their own
fixtures for that reason (`test/integration/harness_test.go:50-56`); mail tests
invoke the consumer callback in-process the same way.

- `TestInboundCreatesConversation` — POST raw `.eml` with a valid HMAC → 200
  fast; run the parse handler; assert conversation, message, snippet, and the
  attachment present in `files` *and* in MinIO.
- `TestInboundIsIdempotent` — same bytes twice → one conversation.
- `TestInboundBadSignatureIsRejected` — 401 and nothing written anywhere.
- `TestInboundUnknownRecipientIsQuarantined` — an event row, no conversation,
  no 500.
- `TestReplyThreadsBack` — reply, assert `In-Reply-To` equals the inbound
  `Message-ID`, then feed the customer's reply to *that* and assert it lands in
  the same conversation.
- `TestReplyCollisionReturns409` — two replies with the same
  `expected_last_message_id` → one 201, one 409 carrying the missed message.
- `TestClaimIsAtomic` — two concurrent claims → exactly one winner.
- `TestOutboundReplyIsSentOnce` — mailpit is already a compose service
  (`deploy/docker/docker-compose.yml:346`, profile `mail`); assert one message,
  correct headers, attachment present.
- `TestBounceMarksMessage` — a DSN to the VERP return path → `delivery_status`
  becomes `bounced` and a timeline event appears; no conversation is created.
- `TestOrphanGCKeepsMailAttachments` — direct regression on §4.2 item 1.

### Tenancy — in the style `tenancy_test.go` already enforces

- `TestMailboxIsWorkspaceScoped` — a member of workspace B cannot list
  workspace A's conversations with a valid uuid.
- `TestConversationRequiresChannelMembership` — a workspace member who is not in
  the mailbox channel gets 403/404, not a conversation.
- `TestMailAttachmentNotReadableCrossChannel` — the `files` download path for an
  email attachment obeys the mailbox channel, mirroring the property
  `internal/file/handler.go:251-264` establishes for chat attachments.
- `TestMailSearchIsACLFiltered` — a conversation indexed with the mailbox
  channel key is invisible to a non-member, in the shape `TestCrossTenantSearch`
  already asserts.

### Operational

The admin DNS-check endpoint is itself the verification story for the parts code
cannot assert, and `mail_inbound_last_received_seconds` is the alert.

---

## 15. Sizing

| Piece | Size |
|---|---|
| 017 schema + repositories | S |
| Ingest accept path | S |
| **MIME parser + corpus + fuzz** | M |
| Threading + merge/split | M |
| `internal/mail` extension + second consumer | M |
| Conversation CRUD, assignment, collision, notes, canned replies | M |
| Admin: domains, verification, DNS, rotation, quarantine | M |
| Search integration | S |
| Retention / object-GC corrections | S |
| **Client** | L |

**Phase total: L**, matching ROADMAP §5. Long pole: the client. Backend long
pole: parser → threading.

---

## 16. Cuts

| Cut | Why |
|---|---|
| Personal mailboxes | ROADMAP §7. XL, no differentiation, competes with clients people love |
| SMTP listener in v1 | A public port-25 listener is an open-relay risk and a second on-call surface in one phase. `Receiver` makes 6b additive |
| Per-provider webhook adapters | The connector treadmill. One raw-MIME + HMAC contract, a documented shim |
| Spam *filtering* | Its own discipline. Verdict consumption only |
| Server-side drafts | Ephemeral presence covers "Jane is replying"; the 409 covers correctness |
| Auto-acknowledgement / auto-responders | Loop risk out of proportion to value, and trivially added later |
| SLA timers, business hours, CSAT, dashboards | Front's surface area; none of it load-bearing |
| Rules / auto-assignment | Phase 7 triggers on `mail.conversation.created`. A seam, not a feature to duplicate |
| Agent-initiated outbound (cold email) | Turns the mailbox into a sending tool; wait until rate limiting and bounce handling have production miles |
| Attachment content extraction for search | Real work, near-zero marginal value over the body text |
| Rich HTML composer | Compose in markdown, render to a small allowlist. A rich HTML editor is a Tier-C editor in disguise |
| Conversation reactions, pins, bookmarks | The chat surface has them; an inbox does not need them |

---

## 17. Open questions

1. **Which inbound path will the first customer actually run** — a provider
   webhook, or MX pointed at the box? It decides whether 6b (the SMTP listener
   and `go-smtp`) is v1 or later, and it is the single largest scope lever here.
2. **Is HTML rendering in scope for v1?** Text-only removes the bluemonday
   dependency and the iframe sandbox entirely, at a real cost to how support
   email looks.
3. **One mailbox per channel, or several mailboxes sharing one channel?**
   (`support@` and `sales@` worked by one team.) The schema currently says one;
   relaxing it later is an index change, tightening it later is a migration.
4. **May a reply add a recipient who was never in the thread?** It changes the
   abuse profile of `write` on a mailbox (§3).
5. **How long are raw RFC822 originals retained?** A compliance question, not an
   engineering one, and it decides whether §13's bucket growth is a bug or a
   policy.
6. **Does plan 00 want a capability that distinguishes "read the inbox" from
   "send from the company domain"?** I have not invented one; see §3.
