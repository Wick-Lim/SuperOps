# Implementation plans

One plan per phase of `../ROADMAP.md`. Each is written to be picked up by an
engineer without its author present: real tables, real routes, the hard part
named, and explicit cuts.

| Plan | Phase | Status |
|---|---|---|
| [00-permissions](00-permissions.md) | Phase 0 — object-level permissions | Design. **Blocks everything.** |
| [01-phase0-remainder](01-phase0-remainder.md) | Phase 0 — unified inbox + audit | **Implemented** (`020`, `021`) |
| [02-drive](02-drive.md) | Phase 1 — Drive + editor registry | Plan |
| [03-work-tracking](03-work-tracking.md) | Phase 2 — issues, boards, cycles | Plan |
| [04-docs](04-docs.md) | Phase 3 — block editor | Plan |
| [05-spreadsheet](05-spreadsheet.md) | Phase 4 — grid + formula engine | Plan |
| [06-design-surface](06-design-surface.md) | Phase 5 — bounded design surface | Plan |
| [07-huddle](07-huddle.md) | — huddles (order-independent) | Plan |
| [08-email](08-email.md) | Phase 6 — shared inbox | Plan |
| [09-workflow](09-workflow.md) | Phase 7 — automation | Plan |

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
| `025`–`029` | Drive + editor registry (plan 02) |
| `030`–`034` | work tracking (plan 03) |
| `035`–`039` | docs (plan 04) |
| `040`–`044` | spreadsheet (plan 05) |
| `045`–`049` | design surface (plan 06) |
| `050`–`054` | huddle (plan 07) |
| `055`–`059` | email (plan 08) |
| `060`–`064` | workflow (plan 09) |

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

Plan 08's list of required edits is **incomplete in the dangerous direction**:
the bucket sweep deletes any object whose key is absent from
`files.storage_key`/`thumbnail_key` (`internal/file/repository.go:100-122`),
and plan 08 stores raw RFC822 originals under a `raw_key` with no `files` row.
As written, every archived email is swept. Plan 08 must add that arm.

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

## Reading order

If you are picking this up cold: `../ROADMAP.md` §1 (why an all-in-one wins or
loses), §3 (the React Native fork), §3b (Drive is the substrate), §3c
(deployment-dependent capabilities), then `00-permissions.md`, then the phase
you are implementing.

The four documents above are the ones that constrain every plan. A plan that
appears to contradict one of them is the plan being wrong, unless it says
explicitly why it is not.
