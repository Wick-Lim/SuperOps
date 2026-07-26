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
