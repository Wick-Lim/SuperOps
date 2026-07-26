# Implementation plans

One plan per phase of `../ROADMAP.md`. Each is written to be picked up by an
engineer without its author present: real tables, real routes, the hard part
named, and explicit cuts.

| Plan | Phase | Status |
|---|---|---|
| [00-permissions](00-permissions.md) | Phase 0 — object-level permissions | **Implemented** (`016`). It blocked everything and no longer does — `acl_object`/`acl_grant`/`acl_key` are applied and every downstream plan shipped on top of them. |
| [01-phase0-remainder](01-phase0-remainder.md) | Phase 0 — unified inbox + audit | **Implemented** (`020`, `021`) |
| [02-drive](02-drive.md) | Phase 1 — Drive + editor registry | **Implemented** (`025`–`028`) |
| [03-work-tracking](03-work-tracking.md) | Phase 2 — issues, boards, cycles | **Partial** (`030`, `031`). Projects, issues, states and ordering work end to end. Labels, cycles, issue relations and project membership are TABLES ONLY — `labels`, `issue_labels`, `cycles` and `issue_relations` have no Go reader or writer, `internal/project` was never built, and `issue_activity` is written and unreadable. See docs/KNOWN-GAPS.md. |
| [04-docs](04-docs.md) | Phase 3 — block editor | **Implemented** (`035`) |
| [05-spreadsheet](05-spreadsheet.md) | Phase 4 — grid + formula engine | **Implemented** — no migration; see ruling 8 |
| [06-design-surface](06-design-surface.md) | Phase 5 — bounded design surface | **Implemented** — no migration; see ruling 8 |
| [07-huddle](07-huddle.md) | — huddles (order-independent) | **Implemented** (`050`). Media is a §3c capability, off by default. Client joins the call on web with `livekit-client` (audio + screen share); mobile gets the bar, the roster and start/end, which is what plan 07's cut actually removes — the mobile dev-build path, not the feature. |
| [08-email](08-email.md) | Phase 6 — shared inbox | **Implemented** (`055`) |
| [09-workflow](09-workflow.md) | Phase 7 — automation | **Implemented** (`060`, `062`, `063`; `061` earmarked, unspent) |

Already shipped or in flight, so not planned here: outbound mail
(`internal/mail`), unified search, the collaboration layer (`internal/collab`)
and SSO (`internal/sso`).

---

## Migration number registry

**The plans disagree about this and the registry wins.** Each was written in
parallel and each independently claimed `017`, so the number stated inside a
plan is indicative of *how many* migrations it expects, not *which*.

Allocation is by block so that a phase can add a migration mid-implementation
without colliding with a phase that has not started:

| Block | Owner |
|---|---|
| `000`–`012` | shipped |
| `013` | free — unused, mail needed none |
| `014` | SSO (`internal/sso`) |
| `015` | collaboration layer (`internal/collab`) |
| `016`–`019` | object permissions (plan 00) — `016` taken (`acl_object`, `acl_grant`, `acl_key`, the two expected-state views and the backfill); `017`–`019` free |
| `020`–`024` | unified inbox + audit (plan 01) — `020` taken (`inbox_events`, `inbox_items`, `notification_prefs`, `inbox_digest_state`, backfill), `021` taken (`audit_logs` → TEXT `resource_id`, monthly partitions, `dedupe_key`, chain, `audit_chain_heads`); `022`–`024` free |
| `025`–`029` | Drive + editor registry (plan 02) — `025` taken (`drive_folders`, `files` reshape, `file_versions`, `workspace_storage`, the `collab_documents` FK, the `workspace` grant subject, both expected-state views), `026` taken (quota: `collab_bytes_at`, `idx_files_workspace`, the `file_versions` backfill and the `bytes_used` re-derivation), `027` taken (trash: `purge_after` and its indexes), `028` taken (sharing: `drive_share_links`, the `link` grant subject, `acl_key_expected` arm 5); `029` free |
| `030`–`034` | work tracking (plan 03, **partially implemented** — see the status table) — `030` taken (the product-wide `comments` table, keyed to `acl_object`), `031` taken (projects, issues, states, labels, cycles, ordering); `033`–`034` free. `032` is HELD for the `p-` project container key and should not be spent on anything else — see ruling 6. |
| `035`–`039` | docs (plan 04) — `035` taken (`file_projections`, `file_projection_refs`, `comment_anchors`, `idx_collab_documents_updated`); `036`–`039` free. **`file_projections` is product-wide**: the spreadsheet and the design surface project into the same table and add none of their own — see ruling 8. |
| `040`–`044` | spreadsheet (plan 05) — **DELIBERATELY UNSPENT.** The spreadsheet's whole backend is a `registry.Kind`; it needs no migration, no route and no table. `040` remains the lowest free number. |
| `045`–`049` | design surface (plan 06) — **DELIBERATELY UNSPENT**, for the same reason. Plan 06's `preview_seq` column is not built: a preview is a thumbnail, and Drive already has one. |
| `050`–`054` | huddle (plan 07) — `050` taken (`huddles`, `huddle_participants`, `huddle_webhook_events`); `051`–`054` free. Adds ZERO arms to `acl_object_expected` and zero to `acl_key_expected`: a huddle is not an ACL object, it is exactly as accessible as its scope. |
| `055`–`059` | email (plan 08) — `055` taken (mail domains, mailboxes, conversations, messages, inbound events, ingest tokens; `files.mail_message_id`; the `idx_files_unowned` rebuild; one new arm in `acl_object_expected`). `acl_key_expected` is UNCHANGED — a conversation is a container and the container arm already inherits its grants. `056`–`059` free. |
| `060`–`064` | workflow (plan 09) — `060` taken (workflows, versions, triggers, runs, step runs, effects, trigger rejections), `062` taken (`workflows.owner_id`, the principal every action authorizes against), `063` taken (`workflow_runs.depth` and `root_run_id`, the loop guard), `064` taken (`collab_documents.repair_requested_at` — the projection sweep's cursor; it belongs to no plan's block and took the lowest free number, which is what Rule 1 asks for). **`061` is EARMARKED and unspent**: it is reserved for the credential vault and schedule columns that arrive with the HTTP node, and must not be taken for anything else. `065`+ free. |

Rules:

1. **Take the lowest free number in your block** and update this table in the
   same change. A block that fills is a signal to talk, not to borrow.
2. **`golang-migrate` runs each file in one implicit transaction.** No
   `CREATE INDEX CONCURRENTLY`, no `ALTER TYPE ... ADD VALUE`, no `VACUUM`.
3. **Every up has a down**, and the down is tested by replaying
   up → down → up against a real database. Where a down is lossy — as
   `009_hardening.down.sql` is, because a hash cannot be reversed — say so at
   the top of the file.
4. Blocks are generous on purpose. Migration numbers cost nothing; a collision
   discovered at merge time costs an afternoon.

---

## Resolved conflicts — these override the plans

The nine plans were written in parallel by authors who could not see each
other's output. They agree on *idiom* — one interface per deployment-dependent
capability, derived-UUIDv5 dedupe ids, keyset `(x, id)` ordering, reuse of
`bindDurable`/`runLoop`/`withSingletonLock` — and disagree wherever two of them
write to the same table. A cross-review found the following. **Where a plan
contradicts this section, this section wins.**

### 1. Notifications — plan 01 owns them (high)

Plans 03, 04, 07 and 08 each extend the existing `notifications` table and its
`notification_type` enum; 03 *drops* the enum that 04 and 08 *alter*. Whichever
landed second would fail outright.

**Resolution:** plan 01 replaces `notifications` with `inbox_events` /
`inbox_items` and is the Phase 0 owner. Pillars add **no** enum values and
**no** columns; they publish `superops.{ws}.inbox.requested` with a `kind`
string and an explicit recipient list. Delete the notification half of plan
03's migration. This must be settled before plan 03 starts, because plan 03 is
sequenced ahead of the rest of plan 01.

**Landed** in migration `020` + `internal/inbox`. The contract a pillar codes
against, verbatim:

```go
// In-process (you already have the fan-out state):
notifier.Deliver(ctx, inbox.Request{
    WorkspaceID: ws,
    Kind:        "issue.assigned",   // '<resource>.<verb>', TEXT, no migration
    ObjectType:  "issue", ObjectID: issueID,   // what it is about
    SubjectType: "issue", SubjectID: issueID,  // what it coalesces under
    ActorID:     actor,
    Title:       "PROJ-14 assigned to you",
    Body:        "by Alice",
    Data:        map[string]string{"issue_id": issueID},
    Recipients:  []string{assignee},           // EXPLICIT, deduped, ≤2000
})

// Out-of-process (you are on the API side, or you have no consumer yet):
inbox.Publish(ctx, natsClient, inbox.Request{ /* same struct */ })
// → superops.{ws}.inbox.requested, consumed by the `inbox-fanout` durable.
// A new pillar adds zero durables and zero lines in cmd/worker.
```

Four rules that are not negotiable, because the badge stops being trustworthy
the moment one of them is broken:

1. **The recipient list is directed and explicit.** No watch/subscribe, no "everyone
   in the container". An undirected event is the channel-unread-badge shape and
   creates the hot-row failure plan 01 names.
2. **Suppression happens before `Deliver`.** Domain-specific mutes (the
   equivalent of `channel_members.muted`) are the producer's job; `in_app=false`
   in `notification_prefs` creates no item at all rather than a hidden one.
3. **`Deliver` is the only writer.** Its `inbox_events` insert is the idempotency
   gate for the coalesced counter; a pillar that writes rows itself re-derives
   that gate and gets it wrong.
4. **If your pillar owns a "mark this container read" action**, call
   `inbox.MarkSubjectRead` inside the same transaction, as
   `channel.Repository.UpdateReadAt` does. That is the only place two unread
   systems are allowed to overlap.

### 2. Orphan GC — plan 02 owns the predicate (high)

Four plans each rewrite one clause of `Repository.ListOrphans`
(`internal/file/repository.go:54-62`), none referencing the others. A missed
clause is not a bug report — it is `runObjectGC` deleting customer data 24
hours after upload.

**Resolution:** plan 02 lands **one** predicate naming every ownership column,
plus the `file_versions` arm, as its first commit, with a regression test.
Later plans add one clause and one test case; nobody rewrites it again.

**Landed** in migration `025` + `internal/file`. Both predicates and the
collector now live in `internal/file` (`repository.go`, `gc.go`); `cmd/worker`
holds only the advisory lock. `ListOrphans` names `folder_id`, `message_id` and
`trashed_at`; `StorageKeysPresent` has arms for `files.storage_key`,
`files.thumbnail_key` and `file_versions.storage_key`.
`TestGCPredicatesFailIfReverted` runs the OLD queries against the SAME fixtures
and asserts they get it wrong, so "a test that fails if the predicate is
reverted" is proven rather than hoped for. **Your arm goes in
`StorageKeysPresent` with a case in `TestStorageKeysPresentCoversEveryReference`
— plan 08's `raw_key` is still missing and is still the dangerous one.**

Plan 08's list of required edits is **incomplete in the dangerous direction**:
the bucket sweep deletes any object whose key is absent from
`files.storage_key`/`thumbnail_key` (`internal/file/repository.go:100-122`),
and plan 08 stores raw RFC822 originals under a `raw_key` with no `files` row.
As written, every archived email is swept.

**Closed** in migration `055`'s own change: `StorageKeysPresent` gained TWO arms
(`mail_messages.raw_key` and `mail_inbound_events.raw_key` — one per table that
can name an object key, which is the invariant), `ListOrphans` gained the
`mail_message_id` clause, `idx_files_unowned` was rebuilt to match it, and
`TestGCPredicatesFailIfReverted` gained a third block that runs the PRE-MAIL
predicates against the same fixtures and asserts they get it wrong.
`TestEveryStorageKeyColumnIsNamedInThePredicate` reads `information_schema` and
now fails by name if a future pillar adds a key column without an arm.

### 3. Access-key encoding — the shipped format wins (high)

Plan 00's original `'ws:<id>:member'` format cannot pass
`internal/search`'s validator, and five plans assumed a reconciliation that was
never specified. **Fixed in plan 00**: the encoding is `<prefix>-<uuid>`, and
`f-<folder>` must be added — together with folder keys in
`search.AccessKeys` — before any editor indexes anything. A dropped key widens
the filter, and a widened tenancy filter is a cross-tenant leak.

### 4. Editor search projections — one design, not three (medium)

Plan 02's registry contract extracts text server-side from a snapshot; all
three editor plans reject that and each invents its own client-published
projection — different tables, different id spaces (`files.id` vs
`collab_documents.id`), different route shapes, same concept.

**Resolution:** this is exactly what ROADMAP §3b exists to prevent. One
projection mechanism belongs in the registry contract (plan 02), keyed
consistently, with each editor supplying only the extraction. Settle it in plan
02 before plan 04 starts; three of these will not converge later.

---

### 5. How a Drive object becomes readable — a grant, not a rule (high)

Decided while implementing `025`, and it constrains every pillar that puts an
object in Drive.

Plan 02 never says what a folder's default permission is, and both obvious
answers are wrong. Deny-by-default (what plan 00 built, and what
`internal/authz`'s tests pin) makes the Drive root invisible to everyone but its
creator. A view arm that makes Drive workspace-readable is a second inheritance
mechanism that only the LIST path knows about — `acl_key` and `Capability` are
computed by different code, so the listing would show what opening refuses, or
the reverse, which is a leak. Both were built and both were wrong.

**Resolution: `acl_grant` gains a `workspace` subject type**, meaning "everyone
in this workspace", with the key prefix `w-` that `KeysFor` already puts in
every member's key set. Creating a workspace's Drive is three writes:

```go
// 1. the row
INSERT INTO drive_folders (workspace_id, name, is_root, created_by) ...
// 2. the ACL object — folders are ACL-NATIVE, so Register owns the path
az.Register(ctx, authz.FolderObject(id), authz.WorkspaceObject(ws))
// 3. the grant that makes it shared — ROOT ONLY; everything below inherits
az.Grant(ctx, actor, authz.WorkspaceSubject(ws), authz.FolderObject(id), authz.CapWrite)
```

Three consequences a pillar has to know:

1. **A workspace grant is capped by the recipient's workspace role** — owner and
   admin get admin, member write, guest read. A grant addressed to "everyone
   here" must not promote anybody past their own role.
2. **Folders are ACL-native and files are derived.** `Register`/`Move` own a
   folder's `acl_object` row; a file's row and path come from
   `acl_object_expected`, which reads the folder's *current* path. Do not put
   folders in that view — that would make them a derived type, and `Register`
   and `Move` both refuse derived types, so Drive could not move a folder.
3. **There is no private folder in v1.** Restricting a subtree means inheritance
   must STOP at a boundary, and inheritance is computed twice — by
   `acl_key_expected` arm 5 in SQL and by `grantedCapability` in Go. Implemented
   in one and not the other it is exactly the bug above. It is a named cut, not
   an oversight; the fix is a boundary expressed once, and it gets its own
   migration.

---

### 6. Plan 03's notification migration is a LIVE TRAP, not a dead letter (high)

Ruling 1 says "delete the notification half of plan 03's migration". That is not
merely advice about a redundant change — the change would still SUCCEED.

Migration `020` deliberately left `notifications` and the `notification_type`
enum intact (see its header), so plan 03 §3's six statements —
`ALTER COLUMN type TYPE TEXT`, `DROP TYPE notification_type`, `ADD COLUMN
object_type`, `ADD COLUMN object_id`, the CHECK and the index — all run without
error against the tree as it stands today. Verified. The result is a table plan
01 owns, silently mutated, and `005_create_notifications.down.sql` broken.

Everything §3 wants already exists: `inbox_events` carries `kind`,
`object_type`, `object_id`, `subject_type` and `subject_id` (migration `020`).
A pillar publishes `inbox.Request` and adds zero durables, zero columns and zero
enum values.

**Landed** as migration `030` + `internal/comment`, which publishes
`comment.mentioned` through `inbox.Publish` and touches no notification table.

### 7. The comment surface is product-wide and it landed FIRST (high)

Plan 02 §13 cut per-file comments explicitly: "Phase 2 builds the comment surface
once and Drive adopts it." Four plans (02, 03, 04, 05) consume it. So it is not
an issue feature and it does not wait for issues — it took the lowest number in
the work-tracking block and shipped alone.

**The target is an `acl_object`,** with a real composite foreign key to
`(object_type, object_id)`. That single decision is what makes it shared:

  * a comment is authorized by its TARGET and has no permission rule of its own.
    `CapComment` is the rung the ladder already carries for exactly this — it
    sits between read and write so somebody can discuss a thing they may not
    change.
  * the FK is enforced. A polymorphic pair with no constraint is the usual shape
    and it rots; here purging a Drive file takes its comments with it, by the
    database rather than by whoever remembers.
  * **a new commentable type is a new `acl_object` type, not a change to
    `internal/comment`.** Register the object and comments work.

Threading is ONE level, enforced by a trigger. Deletion is soft AND blanks the
body — the row survives to hold its replies, and "deleted" has to mean the text
is gone or the feature is a lie the first time somebody reads the table.

Mentions are `<@uuid>`, never `@name`: names are not unique, they change, and
resolving one at read time makes a comment's meaning depend on who is called
what today.

---

### 8. Ruling 4, settled: `ClientProjected` is the mechanism, and `Text` is dead for collab (high)

Ruling 4 said one projection mechanism belongs in the registry contract, "with
each editor supplying only the extraction". It was declared and never built:
`registry.Kind.Text` had zero callers, and its own comment conceded why — a type
can only implement it if it ships a Go-side snapshot reader, and there is no
`io.Reader` that IS a CRDT document.

**It cannot be built in that shape and it will not be.** Reconstructing a
ProseMirror tree from `collab_snapshots` in Go is the same class of work
migration `015` refuses, with the same failure mode: an implementation that
disagreed with the client's would be a corruption bug debuggable from neither
side.

**Landed** as one registry field and one table:

  * `Kind.ClientProjected bool` — the collab half of the mechanism; `Text` is
    the blob half. `validate()` refuses `StorageCollab && Text != nil` and
    `ClientProjected && !StorageCollab`, so an editor inventing its own
    projection is a registration-time error rather than a review comment.
  * `file_projections` (migration `035`) — ONE table, keyed on `files.id`, not
    on `collab_documents.id`. The three editor plans each proposed their own
    table in their own id space; this is the reconciliation.
  * `POST /api/v1/drive/files/{file_id}/projection` in `internal/drive`, not in
    a per-editor package, because docs, spreadsheets and designs need the
    identical thing.

So the spreadsheet and the design surface add **no table, no route and no
migration**. They set one bool and ship one TypeScript extractor. If the backend
for the second editor is not close to zero, the registry did not work.

Five rules make trusting a client-supplied body safe, and each is a test in
`test/integration/projection_test.go` rather than a paragraph here: monotonic on
seq in one statement; never above the log head; authorized on `write` at POST
time over HTTP so a revoked editor cannot land one last rewrite; bounded, with
every limit a refusal rather than a truncation; and never authoritative — losing
the whole table costs search until re-projection and costs zero writing.

---

## Reading order

If you are picking this up cold: `../ROADMAP.md` §1 (why an all-in-one wins or
loses), §3 (the React Native fork), §3b (Drive is the substrate), §3c
(deployment-dependent capabilities), then `00-permissions.md`, then the phase
you are implementing.

The four documents above are the ones that constrain every plan. A plan that
appears to contradict one of them is the plan being wrong, unless it says
explicitly why it is not.
