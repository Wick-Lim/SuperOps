# Plan 00 — object-level permissions

**Phase 0. Blocks every pillar.** Written first because each of the other plans
would otherwise invent its own answer, and reconciling nine of those later is
strictly worse than designing one now.

Status: **done** (`migrations/016`, `internal/authz`, the worker's `acl_drift`
job). Every handler authorizes through `Checker.Can`; the fifteen membership
methods are deleted, and so is the dual-run comparison that made deleting them
safe — see step 5 for why keeping it would have been worse than removing it.

One thing was deliberately left where it was: `KeysFor`'s channel arm still
answers from `channel_members` rather than from `acl_key`. See
"What was not cut over" below.

---

## The problem

`internal/authz` answers two questions well: *is this user in this workspace*
and *is this user in this channel*. Fifteen methods, one query each, and it is
now the single point of every authorization decision in the product.

It cannot answer the questions the next eight pillars ask:

- may this user read *this file*, which lives in a folder shared with them but
  in a workspace they only partially belong to?
- may this user comment on, but not edit, this document?
- what are **all** the objects this user may read, cheaply enough to filter a
  search over 100k of them?
- this person left the company — what did they have access to?

Channel membership is a special case of a general relation. The work is to make
the general case first-class without destabilising the special case that
currently protects the messaging product.

---

## Model

Four concepts. Deliberately four, not more.

**Subject** — who. A user today; a group later; a share-link token eventually.
The model must not assume "user" or adding groups becomes a rewrite.

**Object** — what. `(type, id)`: `channel`, `folder`, `file`, `doc`, `sheet`,
`design`, `issue`, `project`, `mailbox`. Types are open-ended; the ACL layer
must not enumerate them in code.

**Capability** — how much. Ordered, each implying the ones below:

```
admin  >  share  >  write  >  comment  >  read
```

Ordering matters: it makes a check a comparison rather than a set membership
test, and it means a new capability slots in without touching call sites.

**Grant** — an explicit `(subject, object, capability)`. The exception, not the
rule; most access is inherited.

### Effective capability

```
effective(subject, object) = max(
    explicit grants on the object,
    inherited grants from every ancestor,
    capability implied by workspace role
)
```

Deny is *absence*, not a negative grant. Negative grants (`deny read`) look
attractive and are a well-known source of "why can't this person see it"
support burden, because the answer requires evaluating an ordered rule set
rather than a maximum. Excluded on purpose. If an exception is genuinely
needed, the object moves out of the inheriting container.

---

## Storage

Three tables. Postgres only — no new datastore.

**`acl_object`** — the hierarchy.

```
object_type  text
object_id    uuid
workspace_id uuid            -- every object belongs to exactly one workspace
path         text            -- materialized path: '/ws:<id>/folder:<id>/folder:<id>'
PRIMARY KEY (object_type, object_id)
```

Materialized path rather than a closure table: a move is a prefix rewrite of
the subtree (`UPDATE ... WHERE path LIKE '/old/prefix/%'`) instead of deleting
and reinserting O(subtree × depth) closure rows. Postgres indexes the prefix
predicate with a plain B-tree on `text_pattern_ops`. Moves are rare; reads are
constant.

**`acl_grant`** — the exceptions.

```
object_type, object_id
subject_type, subject_id
capability   text
granted_by   uuid
created_at   timestamptz
PRIMARY KEY (object_type, object_id, subject_type, subject_id)
```

One row per (object, subject) — a subject has one capability on an object, the
highest one granted. Storing several and taking the max invites disagreement
about which is current.

**`acl_key`** — the read-path materialization.

```
object_type, object_id
key          text            -- '<prefix>-<uuid>', see below
PRIMARY KEY (object_type, object_id, key)
```

The encoding is **not free**: `internal/search` shipped first and defines it as a
closed set of `<prefix>-<uuid>` (`internal/search/doc.go:105-126`), validated on
both write and query because the Meilisearch filter is built by string
concatenation. `acl_key.key` must store exactly that string, or every search
filter needs a translation layer that can only introduce bugs.

| Prefix | Grants to | Status |
|---|---|---|
| `w-<workspace>` | every member of the workspace | shipped |
| `c-<channel>` | everyone who may read the channel | shipped |
| `u-<user>` | exactly one user — an explicit share, or an owner-only object | shipped |
| `g-<group>` | a group | reserved, unused |
| `f-<folder>` | everyone who may read a folder | added, unused until Drive |

> An earlier draft of this plan specified `'ws:<id>:member'` / `'chan:<id>'` and
> deferred the format to "the wiring pass". That was wrong: the validator is a
> closed set, five plans assumed the reconciliation would happen, and the
> failure mode is not cosmetic — a key that fails validation is dropped, and a
> dropped *narrowing* term widens the query. Widening a tenancy filter is a
> cross-tenant leak.

**`f-` must be added before any editor indexes anything**, together with making
`search.AccessKeys` (`internal/search/handler.go:42-66`) return the folders a
caller can read. Without it, document, spreadsheet and design search either
returns nothing or returns everything, and only one of those is noticed.

Folder keys are bounded by *folders a user can read*, not by objects — that is
the whole reason for the access-key model, and it survives a Drive with 100k
files. If a deployment ever has enough folders per user for the key set itself
to become large, that is the signal to add group keys and grant folders to
groups, not to abandon the model.

This is the part that makes list and search endpoints viable. A caller resolves
to a **set of keys** once per request; "may read" becomes an intersection test,
and a filtered list becomes one indexed query instead of N permission checks.
It is denormalized state, so it must be rebuilt transactionally with whatever
changed it — a grant, a move, a membership change — and there must be a
verification job that recomputes and reports drift, because a silent
divergence here is an access-control bug rather than a stale cache.

> **Seam:** the unified-search work happening now is choosing an access-key
> encoding for the Meilisearch filter. It must be the *same* key set as this
> table. The wiring pass reconciles the exact format; the semantics are defined
> here.

---

## API

Handlers keep calling `internal/authz`; the package grows, and the existing
fifteen methods stay as the channel/workspace special case so the messaging
product does not move while the foundation is replaced underneath it.

```go
// The check. Returns the effective capability, not a bool, so a caller can
// distinguish "may read but not write" without a second query.
func (c *Checker) Capability(ctx, subject, obj ObjectRef) (Capability, error)
func (c *Checker) Can(ctx, subject, obj ObjectRef, want Capability) (bool, error)

// The list path. Resolve once per request, then filter.
func (c *Checker) KeysFor(ctx, subject, workspaceID string) ([]string, error)

// The audit path — "what did this person have access to".
func (c *Checker) GrantsFor(ctx, subject string) ([]Grant, error)
func (c *Checker) SubjectsOf(ctx, obj ObjectRef) ([]Grant, error)

// Mutation. Every one of these rewrites acl_key in the same transaction.
func (c *Checker) Grant(ctx, actor, subject, obj, cap) error
func (c *Checker) Revoke(ctx, actor, subject, obj) error
func (c *Checker) Move(ctx, actor, obj, newParent ObjectRef) error
```

`err != nil` → 500, `!ok` → 403/404, never collapsed — the existing rule,
unchanged and now more important.

---

## Migration

The dangerous part. Multi-tenancy in this repo was broken once and the
integration suite exists because of it; this refactor must not be how it breaks
again.

1. ~~Add the tables.~~ `migrations/016`. Nothing reads them.
2. ~~Backfill.~~ `acl_object_expected` / `acl_key_expected` are the one
   definition of the derived state; the migration's backfill, `Checker.Rebuild`
   and the drift verifier all answer from them, so no two of them can disagree.
   Re-runnable. No behaviour change.
3. ~~Dual-run.~~ `AUTHZ_DUAL_RUN`, on in the integration suite. Each compared
   method also evaluated `Capability` and logged subject, object, both answers
   and the required capability. It changed no answer and could fail no request.
4. ~~Cut over per package, smallest first, only once the comparison is silent.~~
   In order: `emoji`, `presence`, `webhook`, `file`, `search`, `message`,
   `channel`, `workspace`, `admin`+`sso`+`collab`+the WebSocket member checker.
   `block` and `notification` needed nothing — neither ever asked an
   object-shaped question.

   The comparison was extended to run in reverse for this step: `Can` also
   evaluated the predicate its call site used to use, so a converted package
   stayed compared instead of falling out of the comparison the moment it was
   converted. Without that, the counter would have decayed to zero exactly as
   the cutover proceeded and the final report would have read "0 mismatches"
   out of an empty comparison.
5. ~~Delete the old methods last.~~ The six with an object-level equivalent are
   gone: `IsWorkspaceMember`, `IsWorkspaceAdmin`, `IsChannelMember`,
   `IsChannelAdmin`, `CanReadChannel`, `ReadableChannelIDs`. Their predicates
   survive unexported, because `Capability` is composed from them.

   `IsWorkspaceOwner` stays, for the reason it was never compared: the ladder is
   ordered and owner and admin both map to `admin`, so sole ownership is a
   distinguished row rather than a higher rung. `WorkspaceRole` and
   `ChannelRole` stay because a role *name* is not a capability — role-change
   validation compares two roles, which a capability comparison cannot express.
   `AdminWorkspaceIDs`, `SharesWorkspace`, `Channel`, `MessageChannel`,
   `PasswordLoginAllowed`, `SSOEnforced`, `IsBlocked` and `BlockedUserIDs` stay
   because they are lookups or non-ACL domain predicates, not access checks.

   `AUTHZ_DUAL_RUN` and `compare.go` went with them. A comparison needs two
   independent models; after step 5 the only "legacy" side left is the set of
   predicates `Capability` is itself composed from, so it would have compared
   two arrangements of the same primitives and reported agreement forever. That
   is confidence rather than evidence, which is the failure the comparison
   existed to prevent. The equivalence it asserted is now asserted better, in
   `internal/authz`'s own tests: the whole fixture matrix rather than the pairs a
   test run happened to generate, on every `go test`, with no flag.

The existing tenancy tests — `TestCrossTenantSearch`,
`TestCrossTenantChannelAccess`, `TestAdminEndpointsAreWorkspaceScoped`,
`TestCrossChannelIDOR`, `TestWorkspaceRemovalRevokesAccess` — passed unchanged
at every step, as did `TestDeactivationRevokesRefreshToken`. If a step had
required editing one, that would have been the signal that the step changed
behaviour.

### What was not cut over

`KeysFor` resolves the caller's `c-` keys from `channel_members`, not from
`acl_key`, and that survived the cutover on purpose.

A key set is a **narrowing** term. A missing key costs somebody a search result;
an extra key is a cross-tenant read. Staleness produces the second one — a
caller removed from a private channel keeps its `c-` key until the
materialization catches up — and `acl_key`'s derived half is maintained by the
worker's hourly reconcile, not by the write that changed it. An hour of
staleness on a narrowing filter is not a trade worth making to save one indexed
query.

Nothing else depends on that decision, because **no authorization decision about
a workspace, channel or file reads `acl_object` or `acl_key` at all**:
`Capability` resolves those three types from the pre-ACL tables directly, so an
object created one second ago is authorized correctly with no row of its own.
That is what makes a missing materialization row unable to widen anything, and
it is why "register objects on creation" was not needed for correctness.

It was needed for the *watchdog*: nothing wrote those rows, so the drift job
reported every object created since the backfill as `missing_object`, for ever,
growing — and a permanently red watchdog is one nobody reads. The job now
reconciles the derived types (logging exactly what it changed) and then verifies
both halves, so what it reports is drift the reconcile could not explain.

Flipping the channel arm needs the derived materialization to be transactional
with the tables it is derived from: a trigger on `channels` /`channel_members` /
`files`, or a hook on each of the eight write paths that touch them. That has
its own failure mode — a missed hook is a channel that is invisible or
unguarded depending on which arm reads it — and belongs in its own change with
its own migration.

---

## Risks

**N+1 on list endpoints.** The failure mode this design exists to prevent. Any
list endpoint that calls `Can` per row is wrong; it should filter on keys.
Worth a lint or a review checklist, because it will read as correct.

**`acl_key` drift.** Denormalized authorization state. Transactional rebuild
plus a periodic verifier that recomputes and reports mismatches, in the worker
alongside the existing jobs.

**Revocation latency.** ~~A WebSocket subscription authorized at subscribe time
outlives a revoked grant.~~ Closed: `authz.Revoker` is the (subject, object)
seam, `Revoke` drives it per subject over the live objects in the revoked
subtree and `Move` drives it for everyone, both after the transaction commits.
The composition root maps object types onto
`ws.Hub.RevokeChannelSubscription` / `RevokeChannelForAll` and
`collab.Service.RevokeAccess` / `RevokeDocument`, which is what finally gives
the collaboration revocations a caller. The fan-out is capped; past the cap the
socket's periodic recheck is the backstop, and the truncation is logged.

**Deep hierarchies.** A path is text; a pathological nesting depth makes it
long. Cap depth (32 is generous) and reject beyond it.

**Sharing outside the workspace.** Deferred, not designed away: `subject_type`
leaves room for a link token, and `acl_object.workspace_id` stays authoritative
so an external grant can never widen tenancy by accident.

---

## Sizing

**L.** Roughly: schema and backfill S, the checker and key materialization M,
per-package cutover M spread across pillars, verifier and audit surface S.

The cutover is the long pole and it is not parallelizable with itself — but it
is with everything else, because each package moves independently once the
comparison mode is quiet.
