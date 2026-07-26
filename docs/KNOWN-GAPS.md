# Known gaps

What an audit found, what was fixed, and what was deliberately left. Everything
here was established by running something — an exploit, a mutation, a probe —
not by reading.

The purpose of the second list is to stop the next person rediscovering the same
things and to make the reasoning for leaving them explicit. A gap with a stated
reason is a decision; an undocumented one is a bug nobody owns.

## Left deliberately

### Backend surfaces the client cannot reach

These are wired, authorized and tested on the server, and no client calls them.
They are missing UI, not missing behaviour — so the fix is a screen, not a patch,
and it is scoped as product work rather than a defect.

| Surface | State | Consequence today |
|---|---|---|
| `PUT /api/v1/notification-prefs` | Route mounted, enforcement live in `inbox.Notifier` and `inbox.Digester` | Every user is pinned to the default preference ladder. No way to mute a kind or stop digest email. |
| The unified inbox HTTP surface (`GET /inbox`, `/inbox/count`, `/inbox/{id}/events`, `POST /inbox/read-all`, the read/unread/done/undone routes) | All mounted; the worker's fanout, digest and reconcile jobs fill `inbox_items` | Done/undone state, the per-item event trail and per-kind filtering have no way to reach a user. The pillar is served entirely through its own compatibility shim (`RegisterCompatRoutes`), which `app/src/api/notifications.ts` calls. |
| `issue.Repository.Activity` | Written on every state and assignee change (`issue/repository.go`); the reader exists and is unreachable | `issue_activity` accrues rows forever and nothing can display them. |

`app/src/screens/InboxScreen.tsx` is a red herring — it is the *mail* inbox and
imports `mailboxApi`.

### Schema ahead of the product

| Thing | State |
|---|---|
| `labels`, `issue_labels`, `cycles`, `issue_relations` (migration 031) | No writer, no reader, no route. One mention in a comment. `issues.cycle_id` is SELECTed, scanned into the model and shipped in the client type, so it is permanently null through the whole stack. |
| `mailboxes.auto_reply_enabled` / `auto_reply_body` | Read and returned as JSON; nothing writes either and nothing sends an auto-reply. The API always reports `false`. |
| `archived_at` on `projects`, `issues`, `mailboxes` | Filtered on by partial indexes and repository queries; no statement or route ever sets it. The filters are correct for a column that is always null. Only channels have archive routes. |
| `search.TypeIssue` | Declared and accepted by `parseTypes`; no producer. `?type=issue` always returns nothing. Comments and mail are not indexed at all. |
| `folder_id` in the search index | Filterable and documented as backing "search inside this folder"; no handler parameter exposes it and `hitAttributes` omits it. |

### Bulk unindexing blocks the request that caused it

Trashing a folder, restoring one and emptying the trash each publish ONE
synchronous JetStream event per file, inside the HTTP handler, with no cap.
`JetStream.Publish` waits for the storage ack (3 s timeout each), and restore
additionally runs one row read per file.

Measured against localhost NATS: trashing 1 file is 6.5 ms, 150 files is 55.7 ms
— about 330 µs per extra file. Linear and unbounded. With NATS a network hop
away, a large folder is seconds of blocked request, and there is no resumption:
the events not yet published are simply lost when the client gives up on an
operation that has already committed. `internal/drive/events.go` calls this
"bounded by the folder's contents", which is not a bound.

Left because the fix is a queue, not a patch — the events belong on a durable
outbox the worker drains, which is a new mechanism with its own ordering and
retry semantics. Today's failure mode is a slow request and a stale index that
`make reindex` converges; the queue would be a better answer to a problem
nobody has hit yet.

### A read-only viewer absorbs every repair request

`SendToRoomLeader` picks the room member with the lowest connection id, with no
capability filter. But `PutProjection` requires `CapWrite` and the client only
answers when it is editable — so a document sitting open in one read-only
viewer's tab absorbs every `collab.project` request and answers none of them.

Migration 064's cursor means the sweep now moves past it rather than freezing,
so this is "wasted request, document still stale" rather than "queue jammed".
It is new load on an old function: `collab.compact` tolerated a silent leader
because compaction is an optimisation, and repair is not.

The fix is a capability-aware leader election, which means the hub knowing each
member's capability on the document — it currently knows only that they were
allowed to join. Left as a design change rather than guessed at.

### Touch targets below the codebase's own minimum

`components/a11y.ts` exports `MIN_TOUCH = 44` and `touchSlop(size)` precisely so
small controls reach it, and 24 call sites instead use a bare `hitSlop={8}`
around an unpadded `<Text>` at fontSize 13–14 — roughly a 34px target.
`HuddleBar` and `HuddleRoom.web` size their controls to `MIN_TOUCH - 12` (32px).

Left rather than bulk-replaced because the fix cannot be verified without
rendering, and a wrong one is worse than the problem: several of these sit
adjacent in a row (Drive's breadcrumbs, the mail-setup actions), and two
neighbours each growing 13px per side produces overlapping hit areas — a
mis-tap, not a missed one. It needs a pass with a device or a screenshot test,
not a scripted edit.

The unambiguous half IS fixed: Drive's breadcrumbs carried no
`accessibilityRole` and no `accessibilityLabel`, so a screen reader read the
folder names as static text with no indication they could be activated — and on
a phone they are the only way out of a deep folder.

### Mail domain ownership: two holes that need a product decision

Both were demonstrated end to end by an audit. Neither has a fix that is only
code.

**Squatting an address before the domain is registered.** `CreateMailbox`
deliberately permits an address on an unregistered domain — a shared demo
deployment depends on it. So an attacker takes `billing@victim.test` first; the
victim then registers *and verifies* the domain with a real DNS record and is
told everything is fine, while the address stays with the attacker.
`VerifyDomain`'s adoption UPDATE is keyed on the same workspace, so it neither
reclaims the foreign mailbox nor mentions it, and no API path can evict it.

The correct rule is that DNS proof beats first-come: verification should be able
to reclaim the address. Reclaiming means doing something with another tenant's
mailbox and the customer conversations inside it — archive it, rename it,
transfer it — and that is a decision about somebody's data, not a patch.
Refusing verification while a foreign mailbox exists is the other option and
hands the squatter a denial-of-service lever instead.

**Public suffixes.** Registration now requires at least two labels, which stops
`com` from being claimed as the parent of every `.com`. It does not stop
`co.uk`, `github.io` or `herokuapp.com` — telling a public suffix from an
ordinary domain needs a suffix list this deployment does not carry, and
embedding a stale copy of one is its own problem.

**Blast radius, corrected.** An earlier commit message said hijacked mail
"landed in the attacker's mailbox". That is not reachable: `Ingest` scopes the
mailbox lookup to the token's workspace, so the victim's real delivery arrives
on the victim's token, finds nothing, and is accepted-and-dropped with a 202 —
the provider never retries and the sender gets no bounce. The damage is silent,
unbounceable loss of the victim's customer email plus permanent denial of the
address. Worse in some ways than interception, and not the same thing.

**No backfill.** The ownership check runs at registration; nothing scans what is
already stored. A deployment that ran the vulnerable code keeps every overlapping
pair, and longest-match still resolves the subdomain to whoever took it. This
query finds them:

```sql
SELECT a.domain AS child, a.workspace_id AS child_owner,
       b.domain AS parent, b.workspace_id AS parent_owner
  FROM mail_domains a JOIN mail_domains b
    ON a.domain LIKE '%.' || b.domain AND a.workspace_id <> b.workspace_id;
```

### Drive share links are minted, validated, and grant nothing

The whole chain exists except its last link, so this looks finished from every
angle except using it.

`POST /drive/{type}/{id}/links` creates one and grants `LinkSubject(linkID)` on
the object — a real ACL grant, so the *authorization* side is built.
`POST /drive/links/{token}/resolve` verifies the token, checks the password,
expiry, use count and revocation, and returns the object it points at plus
`access_keys: ["l-<linkID>"]`.

**Nothing consumes that key.** There is no middleware, header or session that
turns a resolved token into a subject a request carries, so every subsequent
call is made as whoever the caller already was — anonymous, therefore refused.
Establish it by grepping: the key is produced at `drive/sharing.go` and read
nowhere.

Finishing it means minting a short-lived credential for an unauthenticated
holder and teaching the auth middleware to accept it as `LinkSubject`. That is
new work on the most security-sensitive path in the product, not a patch, so it
is left rather than half-built.

`DriveShareScreen` used to present the token under "Copy this link now — anyone
holding it can open {name}", which was the product promising something it does
not do. The copy now says what is true.

### Audit log trust boundary

Two gaps, referenced from `internal/audit/service.go`.

**The append-only database role is not implemented.** The design calls for the
application to connect as a role with INSERT/SELECT and no UPDATE/DELETE, with
migrations and retention on a separate role. Without it, anything holding the
application's credentials can rewrite history; the per-workspace hash chain and
the off-box anchor make that *evident* rather than impossible, and only up to
`anchored_seq`. Deferred because it needs a second connection pool and a
deployment-time role split, and because the chain already turns silent tampering
into detectable tampering.

**Login events are unreachable through the audit API.** `user.login` and
`user.login_failed` are written with `workspace_id = NULL` and
`chain_seq = NULL` — correctly, since they happen before a workspace is chosen
and the chain is per-workspace. But both `GET /admin/audit-logs` and its export
are scoped to administered workspaces, so those rows appear in neither. The
highest-value audit signal is retrievable only with `psql`. The `/verify`
response does say the chain covers "workspace-scoped entries"; nothing says the
read path does too.

### Architectural, not a patch

**`ws.room.*` and every other NATS subject are unauthenticated inside the
cluster.** A bare `nats.Connect` against the broker can publish a `RoomEnvelope`
and the bridge relays `env.Data` to every local member of that document. The only
guard is `env.OriginID != h.id`.

This is real — a compromised worker, a leaked credential, or any co-tenant
service on the bus can forge frames into any document's room — but it is a
property of the whole bus, not of this subject. Fixing only the room subject
would be theatre. The honest fixes are envelope signing across every bridge
subscription, or per-tenant NATS accounts, and both are deployment-shaped
decisions rather than code changes.

Scope note: it is a WRITE/integrity primitive, not a read one. Delivery is still
gated on the recipient already being in the room, and a leader request addressed
at document X was verified not to reach a client in document Y.

**The indexed ACL is a snapshot with no reconciliation.** `authz.Grant`/`Revoke`
rewrite `acl_key` transactionally and publish no file event, so an object's
indexed ACL refreshes only on its next file event or a full rebuild. The blast
radius is currently nil by accident rather than by design: `EnsureRoot` grants
`CapAdmin` on the Drive root to the *workspace* subject and `DeleteShare` refuses
`subject_type=workspace`, so every Drive object is workspace-readable regardless
and a per-user revoke cannot narrow read access. Message ACLs are resolved
query-side and are immune. Worth revisiting if either of those two facts changes.

**The integration harness's config is a hand-written literal, and only three of
its fields are now pinned to production's.** `buildConfig` in
`test/integration/harness_test.go` does not go through `app.LoadConfig`, so a
default added there reaches every deployment and no test. This has already bitten
twice: `SEARCH_ENABLED`/`FILES_ENABLED` (the file and search routes were silently
unregistered for every run) and the three database timeouts — `lock_timeout`,
`statement_timeout` and `idle_in_transaction_session_timeout` were never
assigned, so they were zero, which `pkg/database` reads as "do not set it". The
suite ran with all three production guards OFF, which inverts what a lock bug
looks like: a route that returns 503 in production hung the suite instead, and a
hang is a CI job killed by the runner with no failing test named.

`TestTheHarnessInheritsProductionDatabaseGuards` now compares those three against
`LoadConfig`'s own output, so they cannot drift again. One consequence is worth
knowing before blaming a change: with `lock_timeout` now at 5s, a test that waits
on a row lock FAILS instead of eventually succeeding.
`TestIssuesAreNotReachableFromAnotherTenant` did exactly that once, at exactly
5.00s, while other test processes were running against the same database. It has
not reproduced: a clean full run sampling `pg_stat_activity` every second found
zero lock waits anywhere except the deliberate 5s one in
`TestAnOperationalFailureIsNotReportedAsInvalid`. Recorded as an observation, not
a diagnosis — running two suites against one database is not a supported
configuration, and the single failure is consistent with that. **The class is not fixed** —
the fourth field to be added will have the same problem. The real repair is for
the harness to call `LoadConfig` and override only what a test genuinely needs
(`Server.Port = 0`, the log level), which was not attempted here because
`validate()` requires `ADMIN_EMAIL`/`ADMIN_PASSWORD` and the harness's env
fallbacks differ from production's, so the change needs CI's env checked
alongside it.

**Share links have no reachable path for a third object type, and `RevokeLink`
carries a branch for one anyway.** `drive_share_links.object_type` has carried
`CHECK (object_type IN ('file','folder'))` since migration 028, and `CreateLink`
is the only INSERT, so `RevokeLink`'s `default` branch cannot be reached. It is
kept deliberately — widening the constraint would otherwise route a new type into
`folder:<thatUUID>`, an `acl_object` row that does not exist, hence `CapNone`,
hence a 404 nobody can explain. Noted because an unreachable branch that is not
documented as such invites deletion.

**`pathSegment` treats an invalid UTF-8 byte two different ways.** Its fast path
returns the string unchanged when there is no control character; the scrubbing
loop, reached when there is one, ranges over the string — so `\xff` survives the
first and becomes U+FFFD in the second. The same byte therefore depends on
whether an unrelated control character happens to be present. Unreachable: the
function only ever sees map keys from `encoding/json`, which substitutes U+FFFD
at decode time, and struct field names, which are ours. Documented in the
function rather than fixed, because a fix would mean scanning every segment for
validity on the hot path to correct a case that cannot occur.

The same caveat applies to `boundPath`'s promise that the message "never carries
a broken sequence" — true for valid UTF-8 input, which is all it can receive.

**The NUL guard's error path was verified by sweep, not by proof.** 1,505 cases —
five value shapes (map, slice, map-in-slice, named key, empty key) at every depth
from 0 to 300, spanning each shape's short-circuit crossover — checking that the
reported path is a suffix of the true one, is marked exactly when truncated, is
valid UTF-8, and never doubles the marker. Zero violations. That is strong
evidence rather than a guarantee: a shape outside those five could in principle
behave differently.

**Vectors the NUL guards do not cover.** `DecodeJSON` covers every JSON body and
`RejectNULInURL` covers the query string and path, but neither reaches:
multipart form fields (safe *incidentally* — `httputil.SanitizeFileName` strips
every rune below 0x20, and a NUL in a multipart filename is refused by Go's own
parser); the seven packages that `json.Unmarshal(msg.Data, …)` from NATS
directly (`internal/notification`, `internal/search`, `internal/inbox`,
`internal/mailbox/outbound.go`, `internal/channel/unread.go`, `internal/thumb`,
`internal/mail/queue.go`), whose content mostly originates from guarded HTTP
requests; and `internal/huddle/handler.go:373`, which reads and unmarshals the
body itself for RTC webhook signature verification. Whether that last one writes
caller strings to Postgres is UNVERIFIED — forging a valid signature was out of
reach. Four routes were never given a verdict at all because the probe's request
shapes were wrong: collab document title, webhook description, issue
title/description, comment body. Assume they behave like the seven that were
measured until checked.

**A `json.RawMessage` in a request struct would be silently skipped.** Verified
directly: `reflect.ValueOf(json.RawMessage{})` has `Kind() == Slice` with
`Elem().Kind() == Uint8`, so the byte-slice fast path returns without walking.
The nuance that makes it a real hazard rather than a theoretical one: a
RawMessage stores the JSON text, so it holds the six-character ESCAPE, not the
byte — `{"data":{"k":"a\u0000b"}}` decodes to `{"k":"a\u0000b"}` verbatim, which
means nothing has gone wrong YET. It becomes a NUL, and a 500, at whatever later
unmarshals it into a string. None exists in HTTP request position today — every
occurrence is a NATS envelope or a response type.

**The URL guard changes what some requests answer**, measured against controls one
byte apart: unauthenticated + NUL gives 400 where the control gives 401; a
nonexistent route gives 400 where the control gives 404; `/health?x=%00` gives
400 where the control gives 200. There is no information oracle — the answer is
identical with and without a token — but it is a client-visible contract change
on paths nobody thinks about. Kept deliberately: a malformed URL is malformed
regardless of what it would have routed to.

**Two assertions depend on invariants they cannot enforce.**
`TestANulRefusalStillCarriesACorrelationID` reads a process-global 4xx counter and
depends on the integration package being sequential — `t.Parallel()` appears
nowhere in it today, but adding it would let another test's 4xx satisfy the delta
while the guard contributes nothing. `TestAWorkflowNameIsStoredTrimmed` seeds a
padded name through `wfRepo.Save`, which works only while trimming lives in the
handler; its `t.Fatalf` diagnoses that rather than blaming the fixture. The
raw-SQL alternative was rejected because `062_workflow_owner.up.sql` adds
`owner_id NOT NULL` with no default, so a hand-written INSERT would break on the
next `ALTER TABLE`.

**`httpClient`'s timeout is asserted, not exercised** — `if httpClient.Timeout <= 0`
pins the value, nothing pins that it fires. That needs a deliberately hanging
endpoint.

**Measuring allocation: count is not size.** `testing.AllocsPerRun` counts
allocations, and a size regression changes their size rather than their number —
it cannot see one, which cost a round to learn. `runtime.MemStats.TotalAlloc` is
the right counter because it is monotonic and GC-independent; `HeapAlloc` reports
live heap and moves under the collector. Five runs is enough: the delta's spread
over seven repetitions is 2,144 bytes at one run, 61 at five, 79 at twenty. And
each of the two allocation tests measures ONE axis — the key-size test uses a
single-level key and would not have caught the quadratic depth regression; the
depth test uses one-character keys and would not catch a key-size one.

**`pg_column_size` inside a CHECK is evaluated pre-TOAST.** The constraint sees
61,952 bytes for `[{"kind":"post_message","config":{"k0":0,…,"k2999":2999}}]`
while `SELECT pg_column_size(steps)` on the stored row reports 21,207. Anyone
measuring headroom against the 64 KiB limit with that query reads about three
times more than there is.

**The share-screen tests cannot fail on behaviour.** All of them in
`app/test/newsurfaces.test.ts` are `fs.readFile` plus a regex over the source
text, because `app/vitest.config.ts` runs `environment: 'node'` with no
testing-library and no react-test-renderer. A change that keeps the text and
inverts the behaviour passes. Every runtime claim about DriveShareScreen in this
repo is therefore read from the source rather than observed — including that a
failed grants request renders nothing affirmative, and that the one-time link
token survives an unrelated failure. Not fixable without adding a render harness,
which is the actual gap.

**The workflow package's NUL checks are unreachable over HTTP and must not be
deleted as dead code.** `httputil.DecodeJSON` answers first, so removing both
`hasNUL` loops from `saveTx` leaves every HTTP workflow test green. They are the
only guard on the exported `Repository.Save`, which the funnel does not sit in
front of — `TestTheRepositoryRefusesNULWithoutTheHTTPFunnel` is what fails.

**The integration suite has grown into its own timeout, and it is sensitive to
accumulated data.** A run on a database carrying a day of test rows (124 MB;
30,111 audit_logs, 28,107 acl_key, 24,046 workflow_runs) took 2,076s and was
killed by `-timeout 35m` with zero test failures — the package timeout, not a
hanging test. The same suite on a fresh database took 1,661s. Nothing regressed:
`TestIssueMoveReordersByNeighbours` alone is 283s clean and 375s dirty, 18% of
the total either way, and every other test scaled by roughly the same 25%.

Two consequences. A local run against a database that has not been recreated
will drift toward the timeout and eventually be killed in a way that names no
test — the same shape as the `http.DefaultClient` hang this branch fixed, from a
different cause. And the suite is one slow test away from needing the timeout
raised in CI too: `TestIssueMoveReordersByNeighbours` is worth looking at before
anything else, since it is a fifth of the wall clock on its own.

**`rbac.RequireWorkspaceRole` is still unwired**, and its own comment says so.
Every workspace-scoped route continues to do its role check handler-locally. The
correctness fix landed; the wiring did not.

## Fixed

Each of these has a test that fails when the fix is reverted. See the commits for
the reasoning; this is the index.

**Cross-tenant, demonstrated by exploit**

- An ingest token filed mail into another tenant's mailbox — the tenancy check
  ran *after* the commit and after attachments were stored, and it burned the
  globally unique `provider_event_id` so the victim's real delivery answered
  "duplicate" and vanished. Now a lookup scope inside the transaction.
- Editing a workflow did not move `owner_id`, so one admin's steps executed with
  another admin's capability. "You cannot automate what you could not do by hand"
  was enforced against the wrong person.
- A mailbox could claim an address on a domain another tenant owns. `mail_domains`
  was globally unique, but the constraint only governed sending, because 055
  reasoned that "receiving is harmless".
- `PATCH /conversations/{id}` assigned to any user id on the deployment.
- `AttachToOutbound` authorized the file but never compared its tenancy to the
  conversation's, so a file's name, type and size rendered in another tenant's
  thread.

**Search told a different story from the database**

- A document was searchable by the text of its FIRST save and nothing after it.
  The JetStream message id was keyed on `files.updated_at`, which a projection
  does not move, so every projection inside the two-minute duplicate window
  collapsed onto the first.
- Trashing a folder left everything inside it searchable; restoring put nothing
  back; purging left orphans that no operation could ever remove, because
  `cmd/reindex` only upserts from live rows and has no prune pass.
- Chat attachments were never indexed by any live path, and `?channel=` returned
  no files, ever.
- Mail attachments: the listing path (`acl_key`) said "the mailbox's grantees"
  and the decision path (`Capability`) said "the uploader alone".

**Reachability**

- The client called `POST /webhooks/{id}/rotate`; the server registers
  `PUT /webhooks/{id}/token`. Rotation is the only remedy for a leaked webhook
  token and it 404'd. `TestEveryClientAPICallHasARoute` now checks all 142 client
  calls against the registered routes.
- A shared inbox could not be shared: `CreateMailbox` writes one grant and the
  sharing route hard-coded folder/file. Offboarding the creator left the mailbox
  reachable by nobody.
- Guests read every workflow definition and run output, including private channel
  ids and the literal bodies automation posts into them.

**Jobs that were documented as running and did not exist**

- `sso.CleanExpiredAuthRequests` / `CleanExpiredPendingLogins` — both said "the
  background worker runs this on a timer"; nothing called either.
- `quota.Recompute` — a drift reconciler with a passing unit test and no caller.
  The green test is exactly what kept the drift invisible.
- Projection repair — the design named a backstop for a document nobody opens and
  neither half existed. The client read the `head_seq`/`projection_seq` gap and
  did nothing with it.
