# Plan 01 — Phase 0 remainder: unified inbox + audit coverage

**Phase 0.** The two items left after permissions (`00-permissions.md`), mail, unified
search, collab and SSO. Both are "cheap now, expensive to retrofit across nine pillars"
work, and both are already half-built in ways that will not survive the second pillar.

Migrations: **017 (inbox), 018 (audit)**. 013 is unallocated in the tree today (000–012,
014 SSO, 015 collab); `golang-migrate` does not care about the gap, and 013/016 are left
for the permissions and search wiring passes.

Status: design. Depends on `00-permissions.md` only for the audit hooks in
`authz.Grant/Revoke/Move`; everything else can start against `main` as it stands.

---

## Part A — Unified inbox

### What it is

One list that answers "what needs my attention", across every pillar, with three states
(unread / read / done), coalesced so a burst is one row, and with per-type delivery
preferences that decide whether each event also becomes a push and whether it also
becomes email — batched into a digest rather than forty separate messages.

The user-visible surface is `GET /api/v1/inbox`: items like *"#alerts — 40 new messages
mentioning you"*, *"PROJ-14 assigned to you by Alice"*, *"3 comments on Q3 Plan"*, each
carrying an object reference the client can route to, each individually markable
read/done, each rolled into the same badge.

**Not in scope:**

- **Watch/subscribe.** Recipients in v1 are *directed*: the person mentioned, the
  assignee, the DM participant, the thread author. "Notify me about everything in this
  doc" is a subscription model (`inbox_subscriptions`, a recipient-expansion step in the
  fan-out) and it belongs with the pillar that first needs it. Cutting it is what keeps
  the fan-out audience an explicit list rather than a query.
- **Replacing channel unread badges.** `internal/channel/unread.go` and the
  `unread-fanout` durable stay exactly as they are. See "the hard part".
- **Per-item snooze / reminders.** Real feature, zero architectural content, add later.
- **Cross-workspace merged view.** Items carry `workspace_id` and the API filters on it;
  the client shows one workspace at a time, as the shell already does.
- **Email *reply* to a notification.** That is inbound mail, i.e. Phase 6.

### What exists, and why it does not generalize

`notifications` (`backend/migrations/005_create_notifications.up.sql:3`) is
`(id, user_id, type, title, body, data jsonb, is_read, created_at)`. Three structural
problems:

1. **No object reference.** The object lives in `data` as
   `{"channel_id":…,"message_id":…}` (`internal/notification/service.go:197`). You cannot
   index on it, cannot coalesce on it, cannot ask "is this notification about a thing I
   can still read".
2. **No workspace.** A notification row is user-scoped only. The workspace exists solely
   in the NATS subject at relay time (`service.go:721`, `workspaceFromSubject`). A
   workspace-filtered inbox is not expressible.
3. **`type` is a Postgres ENUM** with five message-shaped values. Nine pillars means an
   `ALTER TYPE` per pillar, and `migration 005`'s enum already contains a value
   (`channel_invite`) that went unemitted for months.

What *does* generalize and must be preserved:

- **Derived ids as the idempotency mechanism.** `notificationID()`
  (`service.go:42`) hashes `(type, user, subject)` into a UUIDv5, and `Repository.Create`
  is `ON CONFLICT (id) DO NOTHING` returning whether a row appeared
  (`repository.go:36-47`). That boolean is the only thing standing between a JetStream
  redelivery and a second buzz in someone's pocket. **This mechanism is the load-bearing
  part of the whole design below.**
- The fan-out state object (`fanout` at `service.go:135`): author exclusion, block list,
  per-channel prefs, all resolved once per event.
- `channel_members.muted` / `channel_members.notification_pref`
  (`003_create_channels.up.sql:27-28`), honoured by `memberPref.wants` (`service.go:124`).
- Delivery: NATS → `ws.Hub.BroadcastToUser` via `TypeNotificationNew`
  (`internal/ws/relay.go:88`), plus `push.Dispatcher` (`service.go:474`).

### Data model — migration 017

Two tables, not one, and the reason is idempotency rather than taste. Coalescing means
`unread_count = unread_count + 1`, and an increment is *not* idempotent under
at-least-once delivery. The event table restores the property: the insert is the gate,
and only an insert that actually happened is allowed to move the counter.

```sql
-- 017_inbox.up.sql

-- The event. Immutable, one row per (thing that happened, person it happened to).
-- id is derived, exactly as notification ids are today.
CREATE TABLE inbox_events (
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- '<resource>.<verb>', the shape internal/audit already uses for actions.
    -- TEXT + CHECK, not an enum: adding a pillar must not need a migration. Same
    -- argument, and same regex shape, as collab_documents.resource_type
    -- (015_collab.up.sql).
    kind         TEXT NOT NULL
                 CHECK (kind ~ '^[a-z][a-z0-9_]{0,23}\.[a-z][a-z0-9_]{0,23}$'),

    -- What it is about: the message, the comment, the issue.
    object_type  TEXT NOT NULL CHECK (object_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    object_id    UUID NOT NULL,

    -- What it coalesces under: the channel, the doc, the issue. Usually the
    -- object's container; for an issue it is the issue itself.
    subject_type TEXT NOT NULL CHECK (subject_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    subject_id   UUID NOT NULL,

    actor_id     UUID REFERENCES users(id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    body         TEXT NOT NULL DEFAULT '',
    data         JSONB NOT NULL DEFAULT '{}',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- The only read of this table is "the events behind one inbox item, newest first".
CREATE INDEX idx_inbox_events_item
    ON inbox_events (user_id, subject_type, subject_id, created_at DESC, id DESC);
-- Retention sweep.
CREATE INDEX idx_inbox_events_age ON inbox_events (created_at);

-- The item. Mutable, one row per (person, subject). This is what the inbox lists.
CREATE TABLE inbox_items (
    -- Derived UUIDv5 of (user_id, subject_type, subject_id), so it is stable and
    -- usable as the keyset tiebreaker httputil.Cursor requires (pkg/httputil/
    -- pagination.go:23 — the cursor is (CreatedAt, ID) and ID is a single string).
    id           UUID PRIMARY KEY,
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    subject_type TEXT NOT NULL,
    subject_id   UUID NOT NULL,

    state        TEXT NOT NULL DEFAULT 'unread'
                 CHECK (state IN ('unread','read','done')),
    -- The highest-priority reason this item is here; drives the icon and the
    -- kind= filter. Ordered by inbox.kindRank in Go, not by the database.
    top_kind     TEXT NOT NULL,

    unread_count  INT  NOT NULL DEFAULT 0 CHECK (unread_count >= 0),
    event_count   INT  NOT NULL DEFAULT 0 CHECK (event_count  >= 0),
    last_event_id UUID NOT NULL,
    last_actor_id UUID REFERENCES users(id) ON DELETE SET NULL,
    title        TEXT NOT NULL,
    preview      TEXT NOT NULL DEFAULT '',

    first_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    read_at      TIMESTAMPTZ,
    done_at      TIMESTAMPTZ,
    -- Last time this item was carried by a digest. NULL = never mailed.
    notified_at  TIMESTAMPTZ,

    UNIQUE (user_id, subject_type, subject_id)
);

-- The list query: open items for one user, newest activity first.
CREATE INDEX idx_inbox_items_open
    ON inbox_items (user_id, last_at DESC, id DESC) WHERE state <> 'done';
-- The badge, and the digest candidate scan.
CREATE INDEX idx_inbox_items_unread
    ON inbox_items (user_id, last_at DESC, id DESC) WHERE state = 'unread';
-- Workspace-filtered list.
CREATE INDEX idx_inbox_items_ws
    ON inbox_items (user_id, workspace_id, last_at DESC, id DESC) WHERE state <> 'done';
-- Digest sweep: unread, never mailed, quiet long enough. Partial so it stays small.
CREATE INDEX idx_inbox_items_digest
    ON inbox_items (workspace_id, user_id, last_at) WHERE state = 'unread' AND notified_at IS NULL;

-- Per-type delivery preference. Resolution is exact kind > 'prefix.*' > '*'.
CREATE TABLE notification_prefs (
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    kind         TEXT NOT NULL,   -- 'message.mention' | 'message.*' | '*'
    in_app       BOOLEAN NOT NULL DEFAULT TRUE,
    push         BOOLEAN NOT NULL DEFAULT TRUE,
    email        TEXT    NOT NULL DEFAULT 'digest'
                 CHECK (email IN ('never','digest','immediate')),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (user_id, workspace_id, kind)
);

-- Rate ceiling on digests, so a pathological workspace cannot mail hourly forever.
CREATE TABLE inbox_digest_state (
    user_id      UUID NOT NULL REFERENCES users(id)      ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    last_sent_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (user_id, workspace_id)
);
```

**Backfill, in the same migration.** Unread `notifications` rows become items and events;
read rows are left behind and the table is dropped in a later migration once the client
has moved. Two things the backfill must do or the first deploy is an incident:

- Resolve `workspace_id` by joining `data->>'channel_id'` to `channels`. Rows whose
  channel is gone are skipped, not defaulted.
- `UPDATE inbox_items SET notified_at = NOW()` for every backfilled row, so the first
  digest cycle after deploy sees an empty candidate set instead of mailing the entire
  company its notification history.

`notifications` itself is untouched by 017 — the old table and the old route keep working
through the transition.

### The write path

One transaction per (event, recipient), and the shape is the whole design:

```
INSERT INTO inbox_events (...) VALUES (...) ON CONFLICT (id) DO NOTHING
  -> 0 rows: redelivery. Commit and return. No counter moved, no push, no relay frame.
  -> 1 row:  proceed.

INSERT INTO inbox_items (...) VALUES (...)
ON CONFLICT (user_id, subject_type, subject_id) DO UPDATE SET
    state         = CASE WHEN inbox_items.state = 'done' THEN 'unread'
                         WHEN inbox_items.state = 'read' THEN 'unread'
                         ELSE 'unread' END,
    unread_count  = inbox_items.unread_count + 1,
    event_count   = inbox_items.event_count  + 1,
    last_event_id = EXCLUDED.last_event_id,
    last_actor_id = EXCLUDED.last_actor_id,
    last_at       = EXCLUDED.last_at,
    top_kind      = <higher rank of the two, computed in Go and passed in>,
    title         = EXCLUDED.title,
    preview       = EXCLUDED.preview,
    -- A new event makes the item mailable again.
    notified_at   = NULL,
    read_at       = NULL,
    done_at       = NULL
RETURNING unread_count;
```

Push and the WS relay fire **after commit, only when the event insert reported a new
row** — the identical rule `createAndPush` follows today (`service.go:436`).

For a multi-recipient event the fan-out does this once as a **multi-row insert**, not a
loop. The current code loops one INSERT per recipient (`service.go:213-228`); at a
500-member `@channel` that is 500 round trips inside a 25-second handler budget
(`handlerTimeout = consumerAckWait - 5s`, `cmd/worker/main.go:96`). One statement per
event, `unnest`-fed, keeps it flat.

### API surface

Conventions unchanged: `RegisterRoutes(mux, authMw)`, `{data,meta,error}` via
`httputil.JSON`/`JSONList`, keyset cursors via `httputil.ParsePagination`, `err != nil`
→ 500 and `!ok` → 403/404 never collapsed.

```
GET  /api/v1/inbox                    ?state=unread|open|done|all &workspace_id= &kind= &cursor= &limit=
GET  /api/v1/inbox/count              -> {unread, by_workspace:[{workspace_id,unread}]}
GET  /api/v1/inbox/{item_id}/events   keyset over inbox_events for that item
PUT  /api/v1/inbox/{item_id}/read
PUT  /api/v1/inbox/{item_id}/unread
PUT  /api/v1/inbox/{item_id}/done
PUT  /api/v1/inbox/{item_id}/undone
POST /api/v1/inbox/read-all           {workspace_id?, kind?} -> {updated}

GET  /api/v1/notification-prefs       ?workspace_id=
PUT  /api/v1/notification-prefs       [{kind, in_app, push, email}]  (full replace per workspace)
```

`state=open` (the default) is `state <> 'done'`. Every mutation is `WHERE id = $1 AND
user_id = $2` — ownership is the whole authorization story for an inbox item, and it is
enforced in the UPDATE rather than by a separate read.

**Compatibility.** The four existing routes (`internal/notification/handler.go:19-22`)
keep their paths and their JSON shape, and are re-pointed at `inbox_items` projected
into `{id,type,title,body,data,is_read,created_at}`. The RN client ships on its own
schedule; the routes are deleted in the release after it does. `GET
/notifications/unread-count` changes meaning — unread *items*, not unread events — and
that is deliberate: the current count inflates by a factor of the burst size, and the
badge is what the user compares against the list.

### Package layout

| Package | Owns |
|---|---|
| `internal/inbox` (new) | `Event`/`Item` model, `Repository` (the transaction above), `Notifier.Deliver`, `prefs.Resolver`, `Handler`, digest job body. Pillar-neutral: no channel, no message. |
| `internal/notification` (shrinks) | Stays as the *message-domain producer*. Its three JetStream handlers keep the logic that is genuinely message-shaped — DM roster, mention extraction, thread-parent lookup, block list, `channel_members` prefs — and call `inbox.Notifier.Deliver` instead of `Repository.Create`. Its `Repository`/`Handler` move to `inbox`. |
| `internal/push` | Unchanged. `inbox` holds the same `Pusher` interface (`service.go:57`). |
| `internal/mail` | Unchanged. Digest adds three template files and nothing else — `render.go:26` already says "Adding password reset or a digest means dropping in three files", and `Publisher.Queue` (`queue.go:90`) is the send path. |
| `internal/ws` | Unchanged. `TypeNotificationNew` already routes by `user_id` (`relay.go:88`); the digest and item updates ride it. |
| `cmd/worker` | Two new jobs, same `runLoop` + `withSingletonLock` scaffolding: `inbox_digest` (lock `0x50_0004`) and `inbox_reconcile` (lock `0x50_0005`). |

Why a new package rather than growing `notification`: a docs or work-tracking package
importing `internal/notification` to file an inbox event reads wrong, and the fan-out
logic worth keeping there is message-specific. The seam is the point (ROADMAP §1).

**A second entry point for pillars that have no consumer yet:** publish
`superops.{ws}.inbox.requested` with an explicit recipient list, and a single new durable
`inbox-fanout` binds `superops.*.inbox.requested` and calls `Deliver`. A new pillar adds
zero durables and zero worker wiring.

### Delivery preferences, and how they actually reach the fan-out

Resolution order, most specific wins, evaluated in exactly one function:

1. **Channel mute / `notification_pref`** for channel-subject events. Most specific,
   already exists, already works, users already rely on it. Untouched.
2. `notification_prefs` exact `kind`.
3. `notification_prefs` `'<resource>.*'`.
4. `notification_prefs` `'*'`.
5. Built-in default: `in_app=true, push=true, email='digest'`.

The N+1 guard, which is the part that decides whether this survives: the resolver loads
**one query for the whole recipient set** — `WHERE user_id = ANY($1) AND workspace_id =
$2` — into a map, exactly the shape `newFanout` already builds (`service.go:545`). No
per-recipient preference query is permitted anywhere in the fan-out. This is worth a
review-checklist line for the same reason `00-permissions.md` puts one on `Can`-per-row.

`in_app=false` means *no item is created at all*, not a hidden item. A suppressed
notification that still counts toward a badge is the worst of both.

### Digest batching

Job `inbox_digest`, every 5 minutes, advisory-locked so one replica runs it:

1. Select candidate items: `state='unread' AND notified_at IS NULL AND last_at < NOW() -
   INTERVAL '<quiet>'`, grouped by `(user_id, workspace_id)`. The **quiet period**
   (`INBOX_DIGEST_QUIET_PERIOD`, default 10m) is what collapses a burst — an item still
   receiving events is not yet mailed, and any new event resets `notified_at` to NULL and
   `last_at` to now, pushing it back out of the window.
2. Drop users whose resolved `email` preference for the item's kind is `never`. Drop
   users whose `inbox_digest_state.last_sent_at` is inside `INBOX_DIGEST_MIN_INTERVAL`
   (default 1h) — the hard ceiling on mail volume per person.
3. In **one transaction**: claim the items (`UPDATE … SET notified_at = NOW()`), upsert
   `inbox_digest_state`. Commit.
4. Render with `mail.Renderer` and queue with `mail.Publisher.Queue(ctx, workspaceID,
   "digest", msg)`.

**Claim-then-send, not send-then-claim.** If the publish fails after the commit, that
digest is lost and logged. The alternative loses the crash window in the other direction
and mails twice. Losing one digest beats double-mailing a company; say so in the code
comment, because the ordering looks arbitrary otherwise.

**The digest renders in the worker, before queueing.** `mail.Request` deliberately
carries a fully-rendered `Message` so the mail consumer needs no database access
(`queue.go:32-50`). A digest has to aggregate at send time, which looks like a violation
— it is not, because the *digest job* does the aggregating and hands the mail consumer a
finished message. The invariant "nothing unrendered goes on the mail queue" holds.

Content rule: subject + per-item title + count + deep link. Not message bodies. A digest
sits in an external mailbox forever and access can be revoked between claim and delivery;
`preview` text is capped at the same 140-rune budget the push path uses
(`service.go:24`) and omitted entirely for items whose subject is a private channel.

`email='immediate'` exists for the one case that justifies it (a direct mention while
offline) and is gated by the same `INBOX_DIGEST_MIN_INTERVAL` counter, so choosing it
cannot exceed the digest's mail budget.

### The hard part

**One number that is never wrong, produced by an at-least-once pipeline, when three
subsystems already each own part of it.**

The three: `notifications.is_read` (per-event, message-only), `channel_members`
read markers driving `unread.update` (`internal/channel/unread.go`, its own durable
consumer), and now `inbox_items.state`. A user who reads #alerts and still sees "1" on
the inbox bell will not file a bug — they will stop trusting the bell, and then every
pillar that files into it is wasted work.

Three attacks, in order:

1. **The counter can only move when a row was actually created.** The `INSERT … ON
   CONFLICT DO NOTHING` on `inbox_events` is the idempotency gate, in the same
   transaction as the item upsert. This is why there are two tables. It is also why the
   derived-id scheme (`notificationID`, `service.go:42`) must be carried forward verbatim
   rather than reinvented: the namespace UUID and the length-prefixed key encoding are
   both load-bearing, and changing either duplicates every notification once.

2. **One definition of the number, written through from every path that changes it.**
   The badge is `COUNT(*) FROM inbox_items WHERE user_id = $1 AND state = 'unread'` and
   nothing else. The reconciliation rule against channel unread is: **a plain channel
   message never creates an inbox item** — that is the channel badge's job, and it
   already works. Only *directed* events do (mention, DM, thread reply, invite). Then
   `PUT /channels/{id}/read` (`internal/channel/handler.go:46`) additionally marks that
   channel's inbox items read, in the same transaction as the read marker. The two
   systems now cover disjoint things and agree at the one point they overlap. Getting
   this wrong in the other direction — inbox items for every message — is the design that
   looks more "unified" and produces the hot-row failure below.

3. **A verifier, because this is denormalized authorization-adjacent state.**
   `inbox_reconcile` in the worker recomputes `unread_count` and `state` from
   `inbox_events` for a rotating slice of users per run, reports mismatches, and repairs
   them. Exactly the treatment `00-permissions.md` prescribes for `acl_key` drift, for
   the same reason: a silent divergence here is not a stale cache, it is a wrong answer
   to a question the user asked.

### Sequencing

1. **017 + `internal/inbox` repository + the transaction.** Everything blocks on this. **M.**
2. **Rewire the three notification consumers to `inbox.Notifier`.** Behaviour-preserving.
   **The long pole** — not by volume but by risk: it is the one pipeline in the product
   that currently works end to end, and its tests (`handler_events_test.go`,
   `push_test.go`, `test/integration/unread_test.go`) are unforgiving. **M.**
3. `/api/v1/inbox` routes + the compat projection on `/api/v1/notifications`. **S.**
   Parallel with 4 and 5.
4. `notification_prefs` + resolver + fan-out wiring. **S.**
5. Digest job + three mail templates. Needs 1 and 4. **M.**
6. `inbox_reconcile`. **S.**
7. Client: inbox pane, prefs screen. Parallel from step 3. **M.**

### Risks and failure modes

- **Hot item row.** A busy channel is one `inbox_items` row per member, updated on every
  qualifying event. The "directed events only" rule is what keeps this bounded; without
  it, 1000 messages/hour × 500 members is 500k row updates/hour on rows that all share an
  index on `last_at`. If a pillar later wants undirected items (a doc watcher), it needs a
  debounce window in the fan-out before it gets one.
- **Fan-out width.** `@channel` to 5000 people inside a 25-second handler budget
  (`cmd/worker/main.go:96`). One multi-row statement plus one batched push enqueue keeps
  it in milliseconds; a loop does not. Cap the recipient set per event and terminate
  (`PermanentError`) beyond it rather than naking forever.
- **Digest storm on first deploy.** Mitigated only by the backfill setting `notified_at`.
  If that line is dropped from the migration, every user is mailed their entire history
  ten minutes after cutover. Worth an assertion in the migration test.
- **Backfill misresolves workspaces.** Old rows whose channel was deleted have no
  workspace. Skip them; do not invent one.
- **`data` jsonb as an unbounded field.** It is written by every pillar. Cap the
  serialized size (4 KB) and reject beyond it, at the `Deliver` boundary.
- **Preference change does not retroactively apply.** Turning off `message.mention`
  leaves existing items. Correct, and users read it as a bug; the prefs screen says so.
- **A digest referencing an object the user can no longer read.** Deep link 404s. Fine.
  A digest *quoting* content they can no longer read is not; hence titles and counts only.

### Verification

`test/integration/inbox_test.go`, `//go:build integration`, driven through the wired app
as the existing suite is:

- `TestInboxCoalescesBurst` — 40 mentions in one channel → one item, `unread_count` 40,
  one row in `GET /api/v1/inbox`.
- `TestInboxRedeliveryIsIdempotent` — invoke the consumer callback twice with the same
  payload (the harness in `mail_test.go` already shows how to reach the stream directly);
  assert `unread_count` does not double and no second push is enqueued. **This is the
  test that guards the hard part.**
- `TestInboxAgreesWithChannelUnread` — post, mention, `PUT /channels/{id}/read`, assert
  `GET /api/v1/inbox/count` and `GET /channels/{id}/unread` are both zero.
- `TestInboxDoneReopensOnNewEvent`.
- `TestMutedChannelProducesNoInboxItem` — the existing `channel_members.muted` behaviour
  must not regress.
- `TestNotificationPrefsSuppressFanout` — `in_app=false` → no item; `push=false` → item,
  no push; `email=never` → item, no digest claim.
- `TestInboxIsWorkspaceScoped` — into `tenancy_test.go`, alongside
  `TestCrossTenantSearch`: a user never sees an item from a workspace they left.
- `TestDigestBatchesABurst` — 40 events, run the digest job body, assert exactly one
  `mail.Request` on `superops.*.mail.requested` with `Kind == "digest"`, `notified_at`
  set, and a second run producing nothing.

Unit: prefs precedence table test; `itemID`/`eventID` derivation determinism; digest
window selection; `kindRank` ordering. The existing `TestUnreadCounts`
(`integration_test.go:289`) must pass unchanged at every step — if it needs editing, the
step changed behaviour.

---

## Part B — Audit coverage

### What it is

`internal/audit` records eleven actions today, all of them authentication or admin
mutations. As this becomes a company's whole stack, audit stops being a debugging
convenience and becomes the thing an auditor asks for: who read what, who shared what,
who exported what, and can you prove the log was not edited by the person it incriminates.

**Not in scope:** SIEM integration beyond a shipping interface; per-row cryptographic
signatures (key management is a KMS problem and this is self-hosted); WORM storage
guarantees; auditing routine content reads; a compliance-report generator; per-workspace
retention policy (see cuts).

### What must be recorded

Six categories. The first three exist or are cheap; the fourth is the compliance surface;
the fifth is where the volume problem lives.

1. **Authentication and session.** Exists (`internal/auth/service.go:143`, `:185`,
   `:264`, `:396`, `:465`, `internal/auth/totp.go:229`). Add: session revoked, refresh
   reuse detected, SSO login/link (migration 014's `sessions.auth_method` makes the
   distinction available), MFA challenge outcome.
2. **Authorization change.** Grant, revoke, move, role change, membership change. These
   hook **inside** `authz.Grant/Revoke/Move` (`00-permissions.md` API section), not at
   call sites. A pillar cannot forget an audit hook it never had to write.
3. **Sharing.** Share link created / rotated / expired / used, external-subject grant.
   Distinct from (2) because the subject is not a user.
4. **Egress.** File download, export endpoint, bulk search, mail sent to an external
   address, API token issued. This is the category an auditor actually asks about and the
   one with no coverage at all today.
5. **Sensitive reads.** See "the hard part".
6. **Configuration.** SSO provider changes, mail transport changes, retention changes,
   audit sink changes, and **reads of the audit log itself** (`audit.read`, with the
   filter recorded). That last one is the row that catches an administrator going looking.

### Data model — migration 018

Five changes to `audit_logs` (`005_create_notifications.up.sql:33`):

```sql
-- 018_audit.up.sql

-- 1. resource_id becomes TEXT.
--    It is UUID today, and internal/admin/mail.go:244 passes the transport name
--    ("smtp") into it. That INSERT fails with 22P02 on every mail test, and the
--    error is discarded by `_ = h.audit.Log(...)`, so the action has never once
--    been recorded. A live bug, not a hypothetical.
ALTER TABLE audit_logs ALTER COLUMN resource_id TYPE TEXT USING resource_id::text;

-- 2. Coalescing key for repeated reads. Derived UUIDv5 of
--    (actor, action, resource_type, resource_id, date_trunc('hour', now)) —
--    the same technique as notificationID (internal/notification/service.go:42).
--    NULL for events that must never coalesce (every mutation, every auth event).
ALTER TABLE audit_logs ADD COLUMN dedupe_key  UUID;
ALTER TABLE audit_logs ADD COLUMN event_count INT NOT NULL DEFAULT 1;
ALTER TABLE audit_logs ADD COLUMN last_at     TIMESTAMPTZ;
CREATE UNIQUE INDEX idx_audit_logs_dedupe ON audit_logs (dedupe_key) WHERE dedupe_key IS NOT NULL;

-- 3. Tamper-evidence chain, per workspace.
ALTER TABLE audit_logs ADD COLUMN chain_seq  BIGINT;
ALTER TABLE audit_logs ADD COLUMN prev_hash  BYTEA;
ALTER TABLE audit_logs ADD COLUMN hash       BYTEA;
CREATE UNIQUE INDEX idx_audit_logs_chain ON audit_logs (workspace_id, chain_seq)
    WHERE workspace_id IS NOT NULL;

CREATE TABLE audit_chain_heads (
    workspace_id UUID PRIMARY KEY REFERENCES workspaces(id) ON DELETE CASCADE,
    head_seq     BIGINT NOT NULL DEFAULT 0,
    head_hash    BYTEA,
    anchored_seq BIGINT NOT NULL DEFAULT 0,   -- last seq shipped off-box
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- 4. Query indexes. The actor index was dropped in 009_hardening.up.sql:114 as
--    unusable; it comes back with the workspace as the leading column, which is
--    how every query in this table is actually shaped.
CREATE INDEX idx_audit_logs_ws_actor    ON audit_logs (workspace_id, actor_id, created_at DESC);
CREATE INDEX idx_audit_logs_ws_resource ON audit_logs (workspace_id, resource_type, resource_id, created_at DESC);
CREATE INDEX idx_audit_logs_ws_action   ON audit_logs (workspace_id, action, created_at DESC);
-- Deliberately NO GIN on metadata. No query needs it, and it is the most
-- expensive index this table could carry.
```

**5. Partitioning**, in the same migration, by `RANGE (created_at)`, monthly. The cheap
conversion, which is the one to take: add `CHECK (created_at < '<next month>')` to the
existing table, rename it to `audit_logs_<yyyy_mm>`, create the partitioned parent as
`audit_logs`, `ATTACH` the old table as the oldest partition. The lock is `ACCESS
EXCLUSIVE` for the rename and attach — seconds on any realistic table, because no data
moves. Fallback if the CHECK validation scan is too slow: create the parent alongside,
copy in batches, swap, keep the original for one release.

Partitions are created ahead of time by a worker job (`audit_partitions`, hourly,
advisory lock `0x50_0006`) that ensures the next two months exist. A missing partition is
a failed INSERT, i.e. a lost audit record, so the job's failure must be loud —
`health.fail` already surfaces it on `/health`. **Not `pg_partman`**: a new extension in
the Compose and Helm images to replace roughly sixty lines of Go that runs in a loop this
worker already has.

### The chain, concretely

Per **workspace**, not global — a global chain serializes every audit insert in the
deployment. `chain_seq` is advanced under the `audit_chain_heads` row lock:

```
UPDATE audit_chain_heads SET head_seq = head_seq + n ... RETURNING head_seq, head_hash
```

This is exactly the argument `015_collab.up.sql` makes for `collab_documents.head_seq`
over a SEQUENCE — a sequence hands out 5 to a transaction that commits after 6, and a
verifier walking the chain would conclude 5 does not exist. Same failure, same fix, and
the reasoning is already written down in this tree.

`hash = SHA-256(prev_hash || canonical(row))` where `canonical` is a fixed field order,
computed in Go. The chain makes tampering **detectable, not preventable** — anyone with
`UPDATE` on the table can recompute the whole chain — which is why it is only half the
answer.

### Protecting the log from the administrators it audits

Four layers, honestly priced:

1. **Append-only at the database role.** The application connects as a role with
   `INSERT, SELECT` on `audit_logs` and no `UPDATE`/`DELETE`; migrations and the
   retention job run as a separate role. Cheap, real, and it stops the API being the tool.
   It does not stop a DB superuser, and the deploy docs must say so rather than implying
   otherwise.
2. **No API surface that mutates audit.** There is none today and there must never be
   one. Disabling auditing is startup configuration, not a runtime endpoint, so turning
   it off lands in the deploy trail instead of the product.
3. **The chain**, above. Detects in-place edits and deletions.
4. **Off-box anchoring — the layer that actually matters.** Periodically ship
   `(workspace, head_seq, head_hash, at)` somewhere the local administrator cannot
   rewrite. This is a §3c capability, not a feature: one interface, transport chosen by
   config, validated at boot, safe default.

```go
// internal/audit/sink.go — mirrors mail.Sender exactly.
type Sink interface {
    Ship(ctx context.Context, entries []Entry) error
    Name() string
}
```

Transports: `log` (default — the operator's existing log pipeline is usually already
shipped off-box), `file` (append-only path, for a host with an immutable log volume),
`s3` (a MinIO/S3 bucket with object lock and a *write-only* credential distinct from the
files bucket), `http` (a SIEM webhook, with HMAC). Per ROADMAP §3c: the default is safe
rather than convenient, a transport named without its credentials is a **boot failure**
not a first-use failure, and there is an admin-triggered test that ships a real anchor and
reports the real error — the same shape as `POST /api/v1/admin/mail/test`.

A `audit_verify` worker job walks each workspace's chain from `anchored_seq`, reports
breaks, and advances the anchor. A break is a `/health` failure and a log line at ERROR;
it is deliberately not a 500 on any user-facing route, because a corrupted audit log must
not be a denial of service.

### Volume — how it does not become the largest table

The rule that decides it: **routine reads are not audited.** Auditing every
`GET /api/v1/messages` writes 10–100× the row count of `messages` itself, and nobody has
ever read those rows. What is audited is a read that **crosses a sensitivity boundary**:

- a file download or export,
- a read of an object reached through an explicit grant or share link rather than
  through container membership (the distinction `authz.Capability` returns for free once
  `00-permissions.md` lands),
- any read performed by an admin against another user's data, or under impersonation,
- a read of the audit log.

And even those coalesce: `INSERT … ON CONFLICT (dedupe_key) DO UPDATE SET event_count =
event_count + 1, last_at = NOW()`, with `dedupe_key` covering one hour. Fifty downloads
of the same file in an afternoon is a handful of rows with counts, not fifty rows, and the
information an auditor wants survives intact.

Order of magnitude, 500-person workspace: ~1.5k auth events/day, ~500 authorization
changes/day, ~3k egress events/day, ~10k coalesced sensitive reads/day ≈ **15k rows/day,
5.5M/year, roughly 1.5 GB with the jsonb**. Against `messages` at hundreds of thousands
of rows a day. The read rule is the entire difference between 30× smaller and 30× larger.

Retention: `AUDIT_RETENTION_DAYS`, default 365, deployment-wide, enforced by **dropping
partitions**, not by `DELETE`. The message retention job (`cmd/worker/main.go:902`) is
batched, capped and advisory-locked precisely because an unbounded `DELETE` on a large
table was a production problem; partitioning means audit never has that problem at all.

### Write path

Two tiers, because a synchronous `pool.Exec` (`internal/audit/service.go:58`) on a file
download is a latency regression on the hot path:

- **Tier 1 — synchronous, must not be lost.** Authentication, authorization changes,
  sharing, configuration. These stay on `Record`/`Try` as they are.
- **Tier 2 — buffered.** Egress and sensitive reads, through a bounded queue with N
  workers that batch by workspace (batching by workspace is required anyway, because the
  chain lock is per workspace). The shape is `push.Dispatcher` — including its `Dropped()`
  counter (`cmd/worker/main.go:344`) and its drain-before-exit ordering. **A full buffer
  is a counted, logged drop that appears in `/metrics`.** Silently dropping audit records
  is the failure that makes the entire surface worthless, and it is exactly the failure a
  buffer invites.

`metadata` gets a hard serialized cap (4 KB, truncated with a marker) and a documented
rule that it never carries content — message text, file contents, tokens. It is written
by call sites across nine pillars and will otherwise become an accidental content store
with a 365-day retention.

### API surface

The read handler moves from `internal/admin/handler.go:478` to
`internal/audit/handler.go` — the package that owns the table owns the query — mounted at
the same path behind the same `adminMw` (`internal/app/app.go:341`), so nothing about the
authorization changes.

```
GET  /api/v1/admin/audit-logs          ?actor_id= &action= &resource_type= &resource_id= &from= &to= &cursor= &limit=
GET  /api/v1/admin/audit-logs/export   same filters, streams NDJSON
GET  /api/v1/admin/audit-logs/verify   chain status per workspace {ok, head_seq, anchored_seq, breaks:[…]}
POST /api/v1/admin/audit-sink/test     ships a real anchor, returns the transport's real error
```

Workspace scoping stays `h.scope` — the caller sees only workspaces they administer, the
invariant `TestAdminEndpointsAreWorkspaceScoped` covers. `action=` is a prefix match
(`user.`), which the `(workspace_id, action, created_at DESC)` index serves. Export gets
its own rate limiter, the same treatment `RegisterMailRoutes` gives the mail test
(`internal/app/app.go:352-364`), and is itself audited.

### The hard part

**Making audit cheap enough to leave on and trustworthy enough to be worth leaving on —
those two pull in opposite directions.**

Cheap pushes toward: buffer the writes, coalesce aggressively, skip reads, drop under
pressure. Trustworthy pushes toward: write synchronously, never coalesce, record
everything, never drop. Every real audit system that fails, fails by picking one side
silently.

The resolution is to make the trade **explicit and per-category** rather than global,
which is what the two-tier write path and the sensitivity-boundary read rule are. The
things that must never be lost are exactly the things that are low-volume — authentication
and authorization changes are hundreds per day, not millions — so they can afford to be
synchronous and unchained-to-nothing. The things that are high-volume are exactly the
things where an hourly count is as good as individual rows. Nothing in the middle is left
undecided.

The second half is the trust boundary. A tamper-evident chain that lives in the same
database as the thing it protects, guarded by an administrator with `psql`, is theatre.
It becomes real only at the moment the head is anchored somewhere that administrator does
not control, which is why the `Sink` interface is not an optional extra and why its
default (`log`, into a pipeline that in most deployments already ships off-box) has to be
useful rather than a placeholder. If the anchoring is cut, cut the chain with it and stop
claiming tamper-evidence — half of this is worse than none, because it invites a claim
the system cannot support.

### Sequencing

1. **018 partitioning conversion + `resource_id` → TEXT.** The risky one: a live-table
   rewrite. Ships alone, with the fallback path rehearsed. **M.**
2. `audit_partitions` job. Must land with or before 1. **S.**
3. Coalesced writes (`dedupe_key`) + the buffered tier. **M.**
4. New call sites: egress, sharing, sensitive reads, config. Broad, shallow,
   parallelizable across packages. Authorization-change hooks wait on
   `00-permissions.md`. **M.**
5. Chain + `audit_verify` job. **M.**
6. `Sink` interface + transports + admin test route. **S.**
7. Query filters + export. **S.**

Fully parallel with Part A — different tables, different package, no shared code.

### Risks and failure modes

- **The partition conversion.** Everything else here is additive. If the `CHECK`
  validation scan on a multi-million-row table takes an `ACCESS EXCLUSIVE` lock for
  minutes, the deploy stalls. Measure first; the copy-and-swap fallback exists for that.
- **A missing future partition** silently becomes a failed INSERT. Two months of lead
  time and a loud job failure; test the rollover explicitly.
- **Chain lock contention** at very high audit rates for one workspace. Batching by
  workspace means one lock acquisition per batch, not per row. If it ever binds, the
  answer is more batching, not a global chain.
- **The buffer drops under load** exactly when the load is interesting — an incident. The
  `Dropped()` counter must be on `/metrics` and alertable, and Tier 1 must never touch the
  buffer.
- **`metadata` becomes a content store**, then a GDPR deletion request arrives for data
  inside a 365-day-retained append-only table. The 4 KB cap and the no-content rule are
  what prevent it; the alternative is a redaction path through an append-only log, which
  is a contradiction.
- **`Try` swallows failures by design** (`service.go:75`). Correct — a failed audit write
  must not turn a successful login into a 500 — but it means a broken audit path is
  invisible except in logs. Add a counter for audit write failures next to the drop
  counter.
- **Cross-tenant leakage through the export route.** The highest-value target in the
  product: one request, every row. Scoped by `h.scope` like everything else, and it goes
  into `tenancy_test.go` rather than a new file, next to the tests for the bugs that
  already happened once.

### Verification

`test/integration/audit_test.go`:

- `TestAuditMailTestIsRecorded` — the regression for the `resource_id` bug: trigger the
  mail configuration test, assert a `mail.test_sent` row exists. It does not today.
- `TestAuditReadCoalescing` — 50 downloads of one file within the hour → one row,
  `event_count = 50`, `last_at` advanced.
- `TestAuditChainDetectsTamper` — write entries, `UPDATE` one directly, run the verifier,
  assert the break is reported at the right seq.
- `TestAuditPartitionRollover` — advance the partition job, insert with a `created_at` in
  the next month, assert it lands in the new partition.
- `TestRecordSurvivesRequestCancellation` — cancel the request context immediately after a
  failed login; the row still exists. That is `Try`'s stated contract
  (`service.go:75`) and nothing asserts it today.
- `TestAuditExportIsWorkspaceScoped` — into `tenancy_test.go`, extending
  `TestAdminEndpointsAreWorkspaceScoped`.
- `TestAuditSinkAnchors` — with the `log` sink, assert the anchor advances
  `audit_chain_heads.anchored_seq`.

Unit: `canonical()` field-order stability (a change to it invalidates every existing
chain — the test is what makes that a deliberate act); `dedupeKey` hour-bucket boundaries;
`Sink` construction failures are boot failures for every transport.

---

## Cuts

| Cut | Why |
|---|---|
| Watch/subscribe on objects | Recipient resolution becomes a query instead of a list; belongs with the first pillar that needs it |
| Inbox items for undirected channel messages | Duplicates the channel unread badge that already works, and creates the hot-row failure |
| Snooze / remind-me | Product surface, no architecture |
| Per-user "push off" global switch | Was `user_preferences.notifications_push`, dropped as dead schema in `009_hardening.up.sql:247`. Per-kind `push` in `notification_prefs` replaces it properly; a second global switch would just drift |
| Auditing routine content reads | The single decision that keeps `audit_logs` smaller than `messages` |
| Per-workspace audit retention | Retention becomes a batched DELETE inside shared partitions instead of a partition DROP. A self-hosted company's retention policy is a property of the company |
| Per-row cryptographic signatures | Key management is a KMS problem; the chain plus off-box anchoring gets the same property without one |
| GIN index on `audit_logs.metadata` | No query needs it and it is the most expensive index the table could carry |
| Compliance report templates (SOC2/ISO exports) | The data is there; the report is a document, not a system |

**No new Go dependencies.** Everything reuses `pgx`, `nats.go`, `uuid`, `crypto/sha256`
and the existing `internal/mail`, `internal/push`, `internal/ws` and worker scaffolding.
`pg_partman` was considered for partition management and rejected above.

---

## Sizing

| Piece | Size |
|---|---|
| 017 schema + `inbox` repository + the transaction | M |
| Rewire the three notification consumers | M — **long pole** |
| Inbox API + compat projection | S |
| Preferences + resolver | S |
| Digest job + templates | M |
| `inbox_reconcile` | S |
| Client inbox + prefs screens | M |
| 018 partitioning + `resource_id` fix | M — second-riskiest |
| Coalesced writes + buffered tier | M |
| Audit call sites across packages | M |
| Chain + verifier | M |
| `Sink` + transports + admin test | S |
| Audit query filters + export | S |

**Overall L**, and that is a re-pricing: ROADMAP §4 has inbox at M and audit at S. Inbox
holds because the substrate mostly exists. Audit does not: "a handful more `audit.Log`
calls" is S, but "a compliance surface that survives the administrator it audits" means
partitioning, coalescing, a hash chain and an off-box sink, and that is M. Say it now
rather than discovering it in the middle.

The long pole is step A2 — replacing the fan-out under the one pipeline in the product
that already works — and it is not parallelizable with itself. Everything else in both
parts is.
