# Plan 03 — Work tracking

**Phase 2.** Depends on Phase 0 (object permissions, unified search, inbox) and,
softly, on Phase 1 (Drive, for attachments). It is the phase that produces the
**comment surface** every editor phase reuses, so §4 is a cross-phase contract
rather than a local detail.

Written against the working tree at `0d120b7` plus the in-flight SSO/collab/mail
/unified-search work sitting uncommitted beside it.

---

## 1. What it is

Issue tracking for a team that would otherwise use Linear. A **project** (`ENG`)
holds **issues** (`ENG-142`), each with a title, a markdown description, a
**state**, an optional assignee, a priority, labels, an estimate, an optional
**cycle**, an optional parent issue, and **relations** to other issues
(`blocks`, `duplicates`, `relates`). You read them as a **list** or a **board**
grouped by any of state / assignee / priority / label / cycle, filter them, drag
them between and within groups, and save a filter as a **view**. Every issue has
a **comment thread** and an **activity feed**.

It is deliberately not Jira. ROADMAP §5 and §7 are explicit that Jira's
configurability is its cost. The concrete expression of that here: **states are
renamable and reorderable, but every state belongs to one of five fixed
categories** (`backlog`, `unstarted`, `started`, `completed`, `canceled`), and
every product rule — is it done, does it roll over out of a cycle, does it show
in the backlog — keys off the *category*, never the name. A workflow is a
relabelling exercise, not a configuration language.

**Explicitly not in scope** (§12 has the full list with reasons): custom fields,
per-project workflow configuration, per-project permission schemes, time
tracking, burndown, Gantt/timeline, multi-level sub-issues, issue templates,
triage inboxes, SLAs, bulk edit, and issue file attachments.

---

## 2. What it builds on, and what it must not rebuild

| Need | Reuse | Where |
|---|---|---|
| Membership decisions | `internal/authz` | `backend/internal/authz/authz.go:47` — one `Checker`, 15 methods, the single point of every decision |
| Search | `internal/search` | `search.TypeIssue` is *already declared* at `backend/internal/search/doc.go:28`; needs an `IssueDoc` constructor and one `cmd/reindex` source (`cmd/reindex/main.go:229`) |
| Realtime | `internal/ws` | workspace fan-out exists (`internal/ws/hub.go:134`, `:360`); per-connection seq and revocation exist |
| Async work | `cmd/worker` | durable consumers with ack/nak/term (`cmd/worker/main.go:527`), job loops with a singleton advisory lock (`:673`) |
| Envelope, cursors | `pkg/httputil` | `{data,meta,error}` (`pkg/httputil/response.go:21`), keyset `Cursor` (`pkg/httputil/pagination.go:22`) |
| Audit | `internal/audit` | `Service.Log(ctx, ws, actor, action, resourceType, resourceID, meta)` (`internal/audit/service.go:83`) |
| Notifications | `internal/notification` | derived ids make fan-out idempotent (`internal/notification/service.go:~40`) |

Nothing in this phase gets its own index, its own permission cache, its own
event bus, or its own pagination scheme. If a piece of it needs one, that is a
signal the spine is missing something and belongs in Phase 0.

### Two gaps this phase hits immediately

**(a) Plan 00 is not built yet.** `internal/authz` answers workspace and channel
questions only. This plan adds three project-shaped methods to the *same*
`Checker` (§5), following the existing shape, and treats them as the temporary
special case that plan 00's `Capability(ctx, subject, ObjectRef)` replaces. It
does **not** invent an ACL table. When plan 00 lands, `CanReadIssue` becomes a
call to `Capability` and the issue becomes an `acl_object` — the migration path
is plan 00's own dual-run procedure (`docs/plans/00-permissions.md` §Migration).
Private projects are **cut from v1** for exactly this reason: a project readable
by every workspace member needs no new permission dimension, and a private one
is one `acl_grant` row once plan 00 exists.

**(b) `notifications` has no object reference.** ROADMAP §2 names this. Worse,
`notifications.type` is a Postgres ENUM (`migrations/005_create_notifications.up.sql:1`)
and `migrations/009_hardening.up.sql:12` records that golang-migrate wraps every
file in one transaction, so `ALTER TYPE ... ADD VALUE` is not available. Phase 2
therefore takes migration **018**, converts `notifications.type` to `TEXT` with a
`CHECK`, and adds `(object_type, object_id)`. That is Phase 0's unified-inbox
work partially done; whoever owns Phase 0 should take 018 as the starting point
rather than designing a second answer.

---

## 3. Data model

Two migrations. **017** is work tracking; **018** is the comment surface plus the
notification object reference. (Observed on disk: there is no `013`; `014_sso`
and `015_collab` are the in-flight files. 016 is reserved for whatever the
unified-search work takes.)

### 017 — work tracking

```sql
CREATE EXTENSION IF NOT EXISTS btree_gist;   -- for the cycle non-overlap constraint

CREATE TABLE projects (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,          -- 'ENG'
    name          TEXT NOT NULL,
    description   TEXT NOT NULL DEFAULT '',
    lead_id       UUID REFERENCES users(id) ON DELETE SET NULL,
    -- The chat seam, stored rather than derived: a project's discussion channel.
    channel_id    UUID REFERENCES channels(id) ON DELETE SET NULL,
    issue_counter BIGINT NOT NULL DEFAULT 0,
    is_archived   BOOLEAN NOT NULL DEFAULT FALSE,
    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, key),
    CONSTRAINT projects_key_shape CHECK (key ~ '^[A-Z][A-Z0-9]{1,5}$')
);

-- Membership is NOT an access decision (see §2a). It decides who is subscribed
-- by default and who may administer the project.
CREATE TABLE project_members (
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    role       TEXT NOT NULL DEFAULT 'member' CHECK (role IN ('admin','member')),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (project_id, user_id)
);
CREATE INDEX idx_project_members_user ON project_members (user_id);

CREATE TABLE issue_states (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL,
    -- The fixed part. Renaming a state is free; changing what a category means
    -- is not possible, which is the whole anti-Jira guard.
    category   TEXT NOT NULL CHECK (category IN
                 ('backlog','unstarted','started','completed','canceled')),
    position   INT NOT NULL,
    color      TEXT NOT NULL DEFAULT '#6b7280',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, name)
);
CREATE INDEX idx_issue_states_project ON issue_states (project_id, position);

CREATE TABLE labels (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id   UUID REFERENCES projects(id) ON DELETE CASCADE,   -- NULL = workspace-wide
    name         TEXT NOT NULL,
    color        TEXT NOT NULL DEFAULT '#6b7280',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX idx_labels_project_name ON labels (project_id, lower(name))
    WHERE project_id IS NOT NULL;
CREATE UNIQUE INDEX idx_labels_workspace_name ON labels (workspace_id, lower(name))
    WHERE project_id IS NULL;

CREATE TABLE cycles (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    number     INT NOT NULL,
    name       TEXT NOT NULL DEFAULT '',
    starts_at  DATE NOT NULL,
    ends_at    DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, number),
    CHECK (ends_at > starts_at),
    -- Two cycles running at once is a product bug that looks like a data bug
    -- three weeks later. Let Postgres refuse it.
    EXCLUDE USING gist (project_id WITH =, daterange(starts_at, ends_at, '[)') WITH &&)
);

CREATE TABLE issues (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    -- Denormalized from projects. Every tenancy filter, every search doc and
    -- every event subject needs it; joining projects for each is a needless
    -- hop on the hottest query in the phase.
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    number       BIGINT NOT NULL,
    title        TEXT NOT NULL CHECK (char_length(title) BETWEEN 1 AND 512),
    description  TEXT NOT NULL DEFAULT '' CHECK (char_length(description) <= 40000),
    state_id     UUID NOT NULL REFERENCES issue_states(id),
    -- 0 none, 1 urgent, 2 high, 3 medium, 4 low. Sorting is
    -- `NULLIF(priority,0) ASC NULLS LAST` so "none" sinks rather than leading.
    priority     SMALLINT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4),
    assignee_id  UUID REFERENCES users(id) ON DELETE SET NULL,
    creator_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    parent_id    UUID REFERENCES issues(id) ON DELETE SET NULL,
    cycle_id     UUID REFERENCES cycles(id) ON DELETE SET NULL,
    estimate     SMALLINT CHECK (estimate BETWEEN 0 AND 100),

    -- The manual order. COLLATE "C" is load-bearing, not decoration — see §5.
    rank         TEXT COLLATE "C" NOT NULL,

    -- Optimistic concurrency AND the event dedupe key. See §5 and §6.
    version      INT NOT NULL DEFAULT 1,

    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    canceled_at  TIMESTAMPTZ,
    archived_at  TIMESTAMPTZ,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (project_id, number),
    CONSTRAINT issues_rank_bounded CHECK (char_length(rank) BETWEEN 1 AND 64)
);

-- The board's primary access path: one group, in rank order, LIMIT n.
CREATE INDEX idx_issues_board      ON issues (project_id, state_id, rank, id) WHERE archived_at IS NULL;
CREATE INDEX idx_issues_list       ON issues (project_id, rank, id)           WHERE archived_at IS NULL;
CREATE INDEX idx_issues_assignee   ON issues (assignee_id, rank, id)          WHERE archived_at IS NULL AND assignee_id IS NOT NULL;
CREATE INDEX idx_issues_cycle      ON issues (cycle_id, rank, id)             WHERE cycle_id IS NOT NULL;
CREATE INDEX idx_issues_parent     ON issues (parent_id, rank)                WHERE parent_id IS NOT NULL;
CREATE INDEX idx_issues_ws_updated ON issues (workspace_id, updated_at DESC, id);
CREATE INDEX idx_issues_creator    ON issues (creator_id) WHERE creator_id IS NOT NULL;

CREATE TABLE issue_labels (
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    label_id UUID NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, label_id)
);
CREATE INDEX idx_issue_labels_label ON issue_labels (label_id, issue_id);

-- Only the forward edge is stored. `blocked_by` and `duplicated_by` are derived
-- at read time; two rows per relation is two rows to keep in agreement.
-- `relates` is symmetric, so it is stored canonically (source < target) and the
-- CHECK enforces it rather than the application remembering to.
CREATE TABLE issue_relations (
    source_id  UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id  UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    type       TEXT NOT NULL CHECK (type IN ('blocks','duplicates','relates')),
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (source_id, target_id, type),
    CHECK (source_id <> target_id),
    CHECK (type <> 'relates' OR source_id < target_id)
);
CREATE INDEX idx_issue_relations_target ON issue_relations (target_id, type);

-- The seam table. resource_type is validated by shape, not by an enumeration,
-- exactly as collab_documents.resource_type is (migrations/015_collab.up.sql:
-- "adding a fourth editor should not need a migration").
CREATE TABLE issue_links (
    issue_id    UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    object_type TEXT NOT NULL CHECK (object_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    object_id   UUID NOT NULL,
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (issue_id, object_type, object_id)
);
CREATE INDEX idx_issue_links_object ON issue_links (object_type, object_id);

-- The change log. Written in the same transaction as the mutation, so the
-- workflow event in §6 is a projection of a committed row rather than the only
-- record that the change happened.
CREATE TABLE issue_activity (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    issue_id   UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_id   UUID REFERENCES users(id) ON DELETE SET NULL,
    version    INT NOT NULL,
    field      TEXT NOT NULL,     -- state|assignee|priority|cycle|label|title|description|relation|parent
    from_value JSONB,
    to_value   JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_issue_activity_issue ON issue_activity (issue_id, created_at, id);

CREATE TABLE saved_views (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id   UUID REFERENCES projects(id) ON DELETE CASCADE,
    owner_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    name         TEXT NOT NULL CHECK (char_length(name) BETWEEN 1 AND 128),
    is_shared    BOOLEAN NOT NULL DEFAULT FALSE,
    -- A validated filter tree, never SQL. See §5.
    filter       JSONB NOT NULL DEFAULT '{}',
    group_by     TEXT CHECK (group_by IN ('state','assignee','priority','label','cycle')),
    order_by     TEXT NOT NULL DEFAULT 'rank',
    display      TEXT NOT NULL DEFAULT 'list' CHECK (display IN ('list','board')),
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_saved_views_owner ON saved_views (owner_id, created_at DESC);
CREATE INDEX idx_saved_views_shared ON saved_views (workspace_id, project_id) WHERE is_shared;
```

`issues.number` comes from `UPDATE projects SET issue_counter = issue_counter + 1
... RETURNING issue_counter` as the first statement of the create transaction.
This is exactly the pattern `collab_documents.head_seq` uses and for exactly the
reason stated there (`migrations/015_collab.up.sql`, note 1): a `SEQUENCE` hands
out 143 to a transaction that commits after the one holding 144, and `ENG-143`
appearing after `ENG-144` is a support ticket. The row lock serializes creation
within a project, which is fine at human rates — see §10.

`updated_at` gets the same trigger migration 009 group F installs for the
existing tables; reuse that function rather than writing a second one.

### 018 — comments and the notification object reference

```sql
CREATE TABLE comments (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    object_type  TEXT NOT NULL CHECK (object_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    object_id    UUID NOT NULL,
    parent_id    UUID REFERENCES comments(id) ON DELETE CASCADE,
    author_id    UUID NOT NULL REFERENCES users(id),
    body         TEXT NOT NULL CHECK (char_length(body) BETWEEN 1 AND 40000),
    body_type    TEXT NOT NULL DEFAULT 'markdown' CHECK (body_type IN ('markdown')),
    -- Opaque to this package. A doc stores a CRDT relative position, a design
    -- file a canvas point, a spreadsheet a cell ref, an issue nothing. See §4.
    anchor       JSONB,
    reply_count  INT NOT NULL DEFAULT 0,
    resolved_at  TIMESTAMPTZ,
    resolved_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    is_edited    BOOLEAN NOT NULL DEFAULT FALSE,
    is_deleted   BOOLEAN NOT NULL DEFAULT FALSE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    -- Anchors and resolution belong to a thread root, never to a reply.
    CHECK (parent_id IS NULL OR anchor IS NULL),
    CHECK (parent_id IS NULL OR resolved_at IS NULL),
    CHECK ((resolved_at IS NULL) = (resolved_by IS NULL))
);
CREATE INDEX idx_comments_object ON comments (object_type, object_id, created_at, id)
    WHERE parent_id IS NULL;
CREATE INDEX idx_comments_thread ON comments (parent_id, created_at, id)
    WHERE parent_id IS NOT NULL;
CREATE INDEX idx_comments_author ON comments (author_id);

-- The enum cannot be extended (golang-migrate runs each file in one
-- transaction — migrations/009_hardening.up.sql:12), and every pillar after
-- this one needs a new notification type. Convert once, here.
ALTER TABLE notifications ALTER COLUMN type TYPE TEXT USING type::text;
DROP TYPE notification_type;
ALTER TABLE notifications ADD CONSTRAINT notifications_type_valid CHECK (type ~ '^[a-z][a-z0-9_]{0,31}$');
ALTER TABLE notifications ADD COLUMN object_type TEXT;
ALTER TABLE notifications ADD COLUMN object_id   UUID;
CREATE INDEX idx_notifications_object ON notifications (object_type, object_id);
```

---

## 4. The comment surface — a cross-phase contract

Three editors and this phase all need "a threaded discussion attached to a
thing". Built four times it is four threading models, four resolution
semantics and four notification paths. Built once it is `internal/comment`.

**The contract, in five clauses.**

1. **A comment is addressed by `(object_type, object_id)`** — the same
   `ObjectRef` shape plan 00 defines (`docs/plans/00-permissions.md` §Model).
   `internal/comment` knows nothing about issues, documents or spreadsheets.

2. **Authorization is injected, never implemented.** The package takes one
   interface:

   ```go
   // Authorizer is satisfied by authz.Checker once plan 00 lands
   // (Capability(ctx, subject, ObjectRef)); until then each mounting pillar
   // supplies its own adapter.
   type Authorizer interface {
       Capability(ctx context.Context, userID string, obj ObjectRef) (Capability, error)
   }
   ```

   `read` lets you see the thread, `comment` lets you post, `write` lets you
   resolve, author-or-`admin` lets you delete. The ordered capability ladder is
   plan 00's, unchanged. Phase 2 ships an adapter backed by
   `authz.CanReadIssue`; Phase 3 replaces the adapter, not the package.

3. **Threading is one level and copies the messages model.** A root plus
   replies, `reply_count` bumped in the same transaction with the parent
   predicate doubling as validation — the trick at
   `backend/internal/message/repository.go:70-82`, which turns an invalid parent
   into a domain error instead of an opaque FK violation. There is no reason for
   comments to have a *different* threading model from messages.

4. **The package stores anchors; it does not maintain them.** This is the clause
   that makes it reusable. A doc comment is anchored to a text range that moves
   when someone types above it. Rebasing that anchor is a CRDT operation the
   *editor* performs against its own document model — Yjs relative positions
   survive concurrent edits by construction, which is precisely why the
   collaboration layer keeps the CRDT in the client
   (`migrations/015_collab.up.sql`, header). `internal/comment` treats `anchor`
   as opaque JSON and never rewrites it. An editor that finds an anchor
   unresolvable marks it orphaned inside its own `anchor` payload; the comment
   package neither knows nor cares.

5. **Notifications and mentions are emitted by the comment package, resolved by
   the mounting pillar.** A comment publishes
   `superops.{workspace_id}.comment.created` with `{object_type, object_id,
   comment_id, mentions[]}`. The notification service resolves "who else is on
   this thread" from `comments` (which it can, generically) and "who is watching
   this object" through a `Watchers(ctx, ObjectRef) ([]string, error)` callback
   the pillar registers. For issues that is assignee + creator + thread
   participants; for a doc it will be something else.

**Cut for v1: reactions on comments.** The `reactions` table is FK'd to
`messages(id)` (`migrations/004_create_messages.up.sql:22`). Generalizing it
means dropping the FK that currently protects it, on a table in the hot path of
the shipped chat product, for an emoji. Not in this phase.

---

## 5. The hard part — ordering, and the query layer around it

The CRUD is not the problem. The problem is a board grouped by *any* field,
filtered arbitrarily, with drag-and-drop reordering that stays correct when two
people drag at once, and query latency that does not degrade as a project grows.

### 5.1 One rank, not one rank per view

The naive model gives each issue a sort key per ordering context — a
`state_rank`, an `assignee_rank`, a `cycle_rank`. That is Jira's rank-field
approach and it multiplies: N grouping fields × M group values, all needing
maintenance when the grouped field changes.

The observation that collapses it: **a grouped view is a partition of one global
order, and the order within a group is the restriction of the global order to
that group.** So there is exactly one `rank` per issue, ordering all issues in a
project. Grouping by state renders that same sequence in buckets. Dragging
within a bucket computes a rank strictly between the two *visible* neighbours in
that bucket. Dragging across buckets does two things in one transaction: set the
grouped field, and compute a rank between the destination's visible neighbours.

The honest cost of this, stated because it will be noticed: an issue moved
between two group-neighbours passes over any issues of *other* groups that lie
between them globally. In the grouped view that is invisible and correct; in a
flat list ordered by rank it looks like the item jumped further than the user
dragged it. Linear behaves the same way. It is worth one column of docs and not
worth a second rank.

Sub-issue order and cycle order use the same column, for the same reason.

### 5.2 The rank representation

**Fractional index strings** over the base-62 alphabet `0-9A-Za-z` (whose ASCII
order is its lexicographic order). `midpoint(a, b)` returns a string strictly
between `a` and `b`, growing by at most one character per call. `midpoint("", x)`
prepends, `midpoint(x, "")` appends. New issues get `midpoint(head, "")` or
`midpoint("", tail)` depending on where the project puts new work.

Why not the alternatives:

- **Float positions** (Trello's classic model). Double precision exhausts after
  ~52 halvings between the same pair, and the failure is silent: two issues get
  the same float and the order becomes whatever the plan felt like. Fractional
  strings degrade by getting longer, which is observable and fixable.
- **Integers with gaps.** Same failure, sooner.
- **Linked list (`prev_id`/`next_id`).** A move is three writes under a lock and
  a read is a recursive CTE. It also makes "the 500th issue" unreachable without
  walking. No.
- **A sequence CRDT (RGA/LSEQ).** Correct and enormously more machinery than a
  board needs. The collaboration layer exists for documents; a board is not one.

**`COLLATE "C"` is mandatory, and the trap is worse than it looks.** A glibc
`en_US.UTF-8` collation does not sort ASCII-wise: it applies locale rules that
reorder case, so `["A","a","B"]` comes back as `["a","A","B"]`. The index built
on the column agrees with the collation, so nothing errors — the board is
subtly, permanently in the wrong order.

What makes this a production-only bug in *this* repo: `deploy/docker` runs
`postgres:16-alpine`, whose musl libc has a degenerate locale implementation
that collates `en_US.utf8` essentially as C — so the bug does not reproduce in
local development or in the compose stack. `deploy/k8s/helm` uses the Bitnami
postgresql subchart (`values.yaml:288`), which is Debian and glibc, where it
does. "Works on my machine, wrong on the cluster" is the exact shape of failure
worth spending a `COLLATE "C"` on the column, inherited by every index, plus an
integration test (§11) that inserts a set whose C-order and en_US-order differ
and asserts the C-order comes back.

### 5.3 Moves: the client never sends a rank

```
PATCH /api/v1/issues/{issue_id}/move
{ "before_id": "...", "after_id": "...", "state_id": "...", "version": 7 }
```

`before_id`/`after_id` are the ids of the issues the user dropped between, as
the client saw them. The server, inside one transaction:

1. `SELECT pg_advisory_xact_lock(hashtext(project_id))` — serializes moves
   within a project.
2. Reads the *current* ranks of `before_id` and `after_id`, and verifies both
   still belong to the project and (for a grouped drag) to the destination
   group. If either is gone or has moved out, return **409** with the current
   neighbourhood so the client re-renders rather than guessing.
3. Computes `midpoint(before.rank, after.rank)`, writes it plus any field change
   plus an `issue_activity` row, bumps `version`.

Two design points, both load-bearing:

**Neighbour ids, not a computed rank.** If the client computed the midpoint, a
move against a stale view would write a rank derived from a neighbour that had
already moved — landing the issue somewhere plausible but not where it was
dropped, with no way for either side to detect it. Sending intent (`between
these two`) and resolving it server-side against committed state makes the stale
case a 409 instead of silent misplacement.

**The advisory lock, not optimistic retry.** With it, two simultaneous drops
into the same gap produce two distinct ranks in a defined order — the second
computes its midpoint against the first's committed rank. Without it both read
the same neighbours and compute the same string, and the tie is broken
arbitrarily by `(rank, id)`. Moves happen at human drag rates; a per-project
advisory lock held for the duration of one short transaction costs nothing and
removes a class of "why did it land there" reports. As defence in depth the rank
generator appends two random base-62 characters, so even a tie under a skipped
lock yields distinct, still-insertable-between keys.

Ordering is `(rank, id)` everywhere, never `rank` alone — the same reason
`pkg/httputil/pagination.go:22` documents for `(created_at, id)`.

### 5.4 Rank growth and renormalization

Pathological drag patterns (repeatedly moving something to position 2) grow the
rank by roughly one character per move. `issues_rank_bounded` caps it at 64
characters, so the failure mode is a rejected move rather than an unbounded
column. A move that would exceed 56 characters instead enqueues a **project
renormalization**: a new job loop in `cmd/worker` that takes the same per-project
advisory lock, reads every issue in `(rank, id)` order, and rewrites the ranks to
evenly spaced short keys in one transaction. It reuses `withSingletonLock`
(`cmd/worker/main.go:673`) and `runLoop` (`:627`) — the scaffolding exists.
Renormalization is order-preserving by construction, which is the property the
test asserts.

### 5.5 The query layer

**One filter representation, validated, never SQL.** A view's `filter` JSONB is
a conjunction of typed clauses:

```json
{"and":[{"field":"state_category","op":"in","values":["started","unstarted"]},
        {"field":"assignee_id","op":"eq","value":"<uuid>"},
        {"field":"label_id","op":"in","values":["<uuid>"]},
        {"field":"updated_at","op":"gte","value":"2026-01-01T00:00:00Z"}]}
```

`internal/issue/filter.go` compiles this to parameterized SQL through a **closed
whitelist** of `(field, op, value-shape)` triples. An unknown field is a 400, not
an ignored clause — the same fail-closed reasoning `search.ParseObjectType`
applies at `internal/search/doc.go:50` ("an unknown type reaching a query would
be silently dropped from the filter — which *widens* the result set"). There is
no free-text SQL path and no dynamic column names.

**The board is N keyset queries, one per group — not one window function.** The
tempting shape is `ROW_NUMBER() OVER (PARTITION BY state_id ORDER BY rank)` with
an outer `WHERE rn <= 50`. That cannot use the index to stop early: it reads and
ranks every matching row before discarding 95% of them, so board latency grows
with project size instead of with page size. Per-group queries with
`WHERE project_id = $1 AND state_id = $2 AND <filter> ORDER BY rank, id LIMIT 51`
hit `idx_issues_board` and stop at 51 rows regardless of project size. Ten states
is ten cheap indexed queries, issued concurrently against the pool.

**Group keys are resolved first, and capped.** For state, priority and cycle the
group set is small and known. For assignee and label it is unbounded — a 300-person
workspace grouped by assignee is 300 queries. So the board endpoint resolves the
present group keys with one aggregate query over the filtered set, orders them
(state `position`, priority value, member display name, label name), takes the
first 50, and reports `has_more` on the groups themselves. Groups paginate like
everything else.

**Counts are a separate call.** `GET .../issues/count?group_by=…&filter=…`
returns `{group_key: n}` from one `GROUP BY` over the filtered set. Separating it
means the board renders from the cheap query and the column headers fill in a
beat later, instead of the whole board waiting on the aggregate.

**A rank-shaped cursor.** `pkg/httputil.Cursor` is `(time.Time, string)`
(`pagination.go:22`). Rank-ordered lists need `(string, string)`. Add
`httputil.KeyCursor` + `EncodeKeyCursor`/`DecodeKeyCursor` alongside the existing
ones — additive, and it leaves message pagination untouched.

---

## 6. Seams — concretely

The claim in ROADMAP §6 is that work tracking "produces the richest seams". Here
is each one as a mechanism.

### Issue from a message

```
POST /api/v1/projects/{project_id}/issues
{ "title": "...", "source": {"type": "message", "id": "<message_id>"} }
```

The handler resolves the message's channel through
`authz.MessageChannel(ctx, messageID)` (`internal/authz/authz.go:232`) and checks
`CanReadChannel` (`:214`) — never trusting a channel id from the request, which
is the rule that file's header states at `authz.go:15`. On success, in one
transaction: create the issue with the message body as the description, insert
`issue_links(issue_id, 'message', message_id)`, and post a `content_type='system'`
message into the source thread linking back. Clients cannot set `content_type`
to `system` (`internal/message/handler.go:31-34`) — the server can, and this is
what that reservation is for.

**Attachments do not come along.** `files.message_id` is the only linkage a file
has, and `file.Handler.canRead` derives access from it
(described at `internal/search/doc.go:305-310`). Copying a file onto an issue
would create an object whose ACL nothing models. The link points at the message;
real issue attachments wait for Drive (Phase 1). This is a cut, stated in §12.

### Issue linked in a doc

Phase 2's obligation is one endpoint and one search doc:

```
GET /api/v1/issues/{issue_id}/preview
→ {identifier: "ENG-142", title, state: {name, category, color}, assignee, url}
```

authorized per caller, 404 when not readable. A doc stores the issue **id**, not
a snapshot, and renders the unfurl per viewer — so a reader who cannot see the
project sees a dead link rather than a leaked title. The link picker in the
editor is `GET /workspaces/{id}/search?type=issue`, which works the moment
`IssueDoc` exists because `search.TypeIssue` is already a declared type
(`internal/search/doc.go:28`) and the handler already filters by type.

### Workflow triggered on a state change

Every issue mutation publishes durably on the established subject convention
(`superops.{workspace_id}.{resource}.{action}` — `internal/channel/events.go:227`,
`internal/message/handler.go:~104`):

```
superops.{ws}.issue.created
superops.{ws}.issue.updated
superops.{ws}.issue.state_changed     { issue_id, from_state, to_state, from_category, to_category, actor_id, version }
superops.{ws}.issue.assigned
superops.{ws}.issue.commented
```

`state_changed` is a **distinct event carrying both endpoints of the
transition**, not a generic `updated` an automation has to diff. A trigger
predicate reconstructed from a diff is where automation becomes flaky, and the
whole value of Phase 7 is that its triggers are trustworthy.

The dedupe id is derived from `(issue_id, version)`, so a JetStream redelivery
inside the 2-minute duplicate window (`internal/app/app.go:447`) collapses rather
than firing an automation twice. `issue_activity` holds the same transition as a
committed row, so an automation that missed an event can be reconciled instead of
guessed at.

Phase 7 then binds `superops.*.issue.*` as a durable consumer through the
existing `bindDurable` (`cmd/worker/main.go:527`). **No Phase 2 change is
required for it.** That is what "the substrate is already right" means.

### Huddle from an issue

A huddle is authorized against an object. Phase 2's contribution is that the
issue *is* one: `authz.CanReadIssue(ctx, issueID, userID) (bool, error)` in
`internal/authz/authz.go`, following the existing `(bool, error)` convention and
its rule that err and !ok never collapse (`authz.go:11-14`). Huddle then takes
`POST /api/v1/huddles {object:{type:"issue", id}}` and calls it. Phase 2 ships
the check and the `issue_links` row that records the huddle afterwards; it does
not ship the huddle.

### Realtime

Issue mutations broadcast through the hub's workspace fan-out
(`internal/ws/hub.go:360`, `publishDomain`). Payloads are compact —
`{id, project_id, version, fields_changed[]}` — and the client refetches; sending
whole issues to every connection in a workspace is fan-out amplification for no
gain.

There is a real scale objection: a 500-person workspace receives every issue
event for every project. The fix is a **per-project subscription**, and `ws`
already establishes the pattern for it — `room.go:13-21` argues explicitly for a
*separate map per id space* rather than one namespaced key, and `rooms` is that
map for collaboration documents. A `projects` map mirroring it, gated by
`CanReadProject`, is ~60 lines with revocation coming free from the same shape.
**Recommendation: do it, sized S, in the realtime slice** rather than shipping
workspace fan-out and regretting it.

---

## 7. Package layout

| Package | Owns | Notes |
|---|---|---|
| `internal/project` | projects, project members, issue states, labels, cycles | Low-traffic configuration surface. `handler.go`, `repository.go`, `model.go`. |
| `internal/issue` | issues, relations, links, activity, saved views, the board/list query | `rank.go` (pure, no DB, heavily unit-tested), `filter.go` (JSONB → parameterized SQL, closed whitelist), `query.go` (board/list/count), plus the usual handler/repository/model. |
| `internal/comment` | the cross-phase comment surface (§4) | Depends on nothing pillar-specific. Mounted once per pillar with an `Authorizer` and a `Watchers` callback. |
| `internal/authz` | **grows** three methods | `ProjectRole`, `CanReadProject`, `CanReadIssue`. Same file, same conventions. Not a new package — the point of `authz` is that there is one. |
| `internal/search` | **grows** `IssueDoc` + `Indexer.HandleIssue` | `IssueDoc.Doc()` returns ACL `[WorkspaceKey(workspace_id)]` while projects are workspace-readable; when plan 00 lands it becomes the object's key set. |
| `internal/notification` | **grows** `HandleIssueEvent`, `HandleCommentEvent` | Derived notification ids keyed on `(type, user, issue, version)` so redelivery collapses but a genuine second assignment still notifies. |
| `cmd/worker` | **grows** two durables + one job loop | `indexer-issue` (`superops.*.issue.*`), `notifier-issue` (`superops.*.issue.*`, `superops.*.comment.created`), and the rank renormalizer. |
| `cmd/reindex` | **grows** one `source` entry | `cmd/reindex/main.go:229` — its own comment says adding a type is "a doc constructor plus one entry here". |
| `pkg/httputil` | **grows** `KeyCursor` | §5.5. |

**No new Go dependencies.** Fractional indexing is ~120 lines of string
arithmetic; importing a library for it would be more code read than written. One
new *Postgres extension*, `btree_gist`, for the cycle non-overlap `EXCLUDE`
constraint — it is a contrib module already present in `postgres:16-alpine` and
is added in migration 017 alongside the existing `uuid-ossp`/`pgcrypto`/`pg_trgm`
(`migrations/000_extensions.up.sql`). If the deployment story makes an extra
extension unwelcome, drop the constraint and enforce non-overlap in the
repository; say so at review time rather than discovering it in Helm.

---

## 8. API surface

Conventions throughout: `RegisterRoutes(mux, authMw)`, `{data,meta,error}`
envelope, keyset cursors, authorization as the first statement of the handler
body, `err != nil` → 500 and `!ok` → 403/404 never collapsed.

```
# projects, states, labels, cycles  (internal/project)
GET    /api/v1/workspaces/{workspace_id}/projects
POST   /api/v1/workspaces/{workspace_id}/projects
GET    /api/v1/projects/{project_id}
PATCH  /api/v1/projects/{project_id}
DELETE /api/v1/projects/{project_id}                    # archive
GET    /api/v1/projects/{project_id}/members
POST   /api/v1/projects/{project_id}/members
DELETE /api/v1/projects/{project_id}/members/{user_id}
GET    /api/v1/projects/{project_id}/states
POST   /api/v1/projects/{project_id}/states
PATCH  /api/v1/projects/{project_id}/states/{state_id}
DELETE /api/v1/projects/{project_id}/states/{state_id}  # 409 if issues remain
GET    /api/v1/workspaces/{workspace_id}/labels
POST   /api/v1/workspaces/{workspace_id}/labels
PATCH  /api/v1/labels/{label_id}
DELETE /api/v1/labels/{label_id}
GET    /api/v1/projects/{project_id}/cycles
POST   /api/v1/projects/{project_id}/cycles
PATCH  /api/v1/cycles/{cycle_id}

# issues  (internal/issue)
GET    /api/v1/projects/{project_id}/issues            # ?filter=&group_by=&limit=&cursor=
GET    /api/v1/projects/{project_id}/issues/count      # ?filter=&group_by=
POST   /api/v1/projects/{project_id}/issues
GET    /api/v1/issues/{issue_id}
GET    /api/v1/issues/{issue_id}/preview               # the doc-unfurl seam
PATCH  /api/v1/issues/{issue_id}                       # body carries `version`; 409 on stale
DELETE /api/v1/issues/{issue_id}                       # archive
PATCH  /api/v1/issues/{issue_id}/move                  # §5.3
GET    /api/v1/issues/{issue_id}/activity
GET    /api/v1/issues/{issue_id}/children
POST   /api/v1/issues/{issue_id}/labels                # {label_id}
DELETE /api/v1/issues/{issue_id}/labels/{label_id}
GET    /api/v1/issues/{issue_id}/relations
POST   /api/v1/issues/{issue_id}/relations             # {type, target_id}
DELETE /api/v1/issues/{issue_id}/relations/{target_id}/{type}
GET    /api/v1/issues/{issue_id}/links
POST   /api/v1/issues/{issue_id}/links                 # {object_type, object_id}
DELETE /api/v1/issues/{issue_id}/links/{object_type}/{object_id}

# saved views  (internal/issue)
GET    /api/v1/workspaces/{workspace_id}/views
POST   /api/v1/workspaces/{workspace_id}/views
PATCH  /api/v1/views/{view_id}
DELETE /api/v1/views/{view_id}

# comments — generic, mounted per object type  (internal/comment)
GET    /api/v1/{object_type}/{object_id}/comments      # roots, keyset
POST   /api/v1/{object_type}/{object_id}/comments
GET    /api/v1/comments/{comment_id}/replies
POST   /api/v1/comments/{comment_id}/replies
PATCH  /api/v1/comments/{comment_id}
DELETE /api/v1/comments/{comment_id}
POST   /api/v1/comments/{comment_id}/resolve
DELETE /api/v1/comments/{comment_id}/resolve
```

`{object_type}` in the comment routes is matched against the registry of types
that have registered an `Authorizer`; an unregistered type is a 404 before any
query runs. `http.ServeMux` will route `/api/v1/{object_type}/{object_id}/comments`
against literal-prefixed patterns correctly (more specific patterns win), but the
registration is worth a startup assertion — this repo has already had a boot
panic from ambiguous route patterns (`b9ac800`).

---

## 9. Sequencing

**Ships first, blocks everything:** migration 017, `internal/issue` model and
repository, and `rank.go`. `rank.go` is pure and gets its unit tests before any
handler exists — it is 120 lines that every later query depends on being right.

**Then, in order:**

1. Issue CRUD + `authz` project methods + audit wiring. (Everything else needs a
   readable issue.)
2. `query.go` + `filter.go` + the board and list endpoints. **The long pole.**
3. `move` + the advisory lock + the renormalizer job.
4. Client: list view, then filters, then board, then drag. Drag is the expensive
   client piece; ROADMAP §3 puts these surfaces web-first, so mobile gets list
   and detail and no drag layer.

**Parallel from day one** (shares only the migration file):

- `internal/comment` + migration 018. A different engineer, no dependency on the
  issue query layer.
- Events + `IssueDoc` + `cmd/reindex` source + the two worker durables. Depends
  only on the issue *model* landing, not on the views.

**Parallel once events exist:** notifications, the message→issue seam, the
preview endpoint, the `ws` project subscription.

**Last:** saved views (trivial once `filter.go` is validated), cycles UI.

The long pole is **the board end to end** — server query shape, rank semantics
and client drag — not the CRUD. Schedule against that.

---

## 10. Risks and failure modes

**Collation.** Covered in §5.2. The worst property of this bug is that it
produces no error, only a wrong order, on a subset of deployments. It is the
first integration test.

**Rank ties under concurrent drops.** Mitigated by the advisory lock and the
random suffix; the residual is an arbitrary-but-stable order between two
simultaneous drops into one gap. Acceptable, and documented in the API.

**Stale-neighbour moves.** Turned into a 409 by construction (§5.3). The failure
to watch for is a client that retries the 409 blindly with the same neighbour
ids and loops.

**Board fan-out.** Grouping by assignee in a large workspace is bounded by the
50-group cap, but the cap is a product decision surfacing as `has_more` on
groups — the client must render it, or users conclude issues are missing.

**Filter compilation.** The single highest-risk code in the phase, because it
turns caller-controlled JSON into SQL. Closed whitelist, parameterized values,
no dynamic identifiers, table-driven tests over every `(field, op)` pair plus a
fuzz corpus of malformed trees. A saved view is stored JSONB, so a filter that
was valid when saved must still be rejected safely if a field is later removed.

**Notification storms.** A bulk state change or an automation touching 200 issues
produces 200 notifications. Derived ids (`(type, user, issue, version)`) stop
redelivery duplicates but not genuine volume. v1 answer: notify only on
assignment, mention, and comment — not on every field change. Coalescing is a
Phase 0 inbox concern and belongs there.

**`issue_counter` serialization.** Creation within a project is serialized by a
row lock. At human rates that is invisible; a CSV import of 10k issues is 10k
serialized transactions. If import ships, it takes the lock once and allocates a
block of numbers — but import is out of scope for v1.

**Description editing is last-write-wins.** `version` turns a concurrent edit
into a 409 rather than silent loss, but two people writing a long description
still get a fight. The forward path is already in the schema:
`collab_documents.resource_type` is validated by shape specifically so a fourth
editor needs no migration (`migrations/015_collab.up.sql`), so
`('issue_description', issue_id)` becomes a collaborative document in Phase 3
with no schema change here.

**Archived vs deleted.** Every board index is partial on `archived_at IS NULL`.
An issue that is archived and then queried by id must still resolve (links from
docs and messages outlive archiving), so the detail path must not inherit the
partial predicate. This is the kind of asymmetry that produces a 404 on a link
someone pasted six months ago.

**Search reindex.** Issues only enter the index through the JetStream event path.
A Meilisearch restore or an index rebuild needs the `cmd/reindex` source; without
it issues are silently unsearchable while everything else works. Ship the source
in the same slice as the indexer, not after.

---

## 11. Verification

Unit, no infrastructure:

- `rank_test.go` — `midpoint` produces a strictly-between value for 10k random
  pairs; repeated left-insertion grows length sub-quadratically; the generated
  alphabet is closed; `midpoint` is never equal to either endpoint.
- `filter_test.go` — every whitelisted `(field, op)` compiles to the expected SQL
  and args; every unknown field/op is a 400; a fuzz corpus of malformed trees
  never panics and never emits an unparameterized value.

Added to `backend/test/integration` (real Postgres/Redis/NATS, Meili/MinIO
optional — `harness_test.go:50-56`):

- `TestIssueRankCollationIsC` — insert ranks whose C-order and `en_US` order
  differ; assert the board returns C-order. The regression test for §5.2.
- `TestConcurrentMovesPreserveEveryIssue` — 20 goroutines dropping into the same
  gap; assert every issue appears exactly once, no rank exceeds the cap, and two
  successive board reads return the same sequence.
- `TestBoardGroupIsRestrictionOfGlobalOrder` — for a filtered board, each group's
  sequence equals the global rank order filtered to that group.
- `TestMoveAgainstStaleNeighbourIs409` — move B away, then drop A next to it.
- `TestRankRenormalizationPreservesOrder` — drive ranks near the cap, run the
  worker job, assert order is unchanged and lengths collapse.
- `TestCrossTenantIssueAccess` — the sibling of `TestCrossTenantChannelAccess`
  (`tenancy_test.go:88`): a member of workspace A cannot read, patch, move,
  comment on, or preview an issue of workspace B, by id.
- `TestIssueFromMessageRequiresChannelRead` — a user who cannot read a private
  channel cannot mint an issue from one of its messages. The IDOR test for the
  richest seam.
- `TestSavedViewFilterRejectsUnknownField` — a stored view with a field removed
  from the whitelist fails closed.
- `TestIssueStateChangeEmitsDurableEvent` — subscribe to
  `superops.*.issue.state_changed`, assert `from_state`/`to_state` and that a
  duplicate publish inside the dedupe window yields one delivery.
- `TestIssueSearchIsACLFiltered` — index an `IssueDoc`, assert a non-member gets
  nothing. Skips when Meili is disabled, exactly as the existing search tests do.
- `TestCommentAuthorizerIsEnforced` — comment on an object whose `Authorizer`
  returns `read`; assert 403 and that a database error from the `Authorizer`
  surfaces as 500 rather than 403.

Opt-in, behind `-tags=bench`: seed 50k issues across 10 states, assert a filtered
board render (10 group queries + one count) stays inside a stated budget, and
`EXPLAIN` shows `idx_issues_board` with no sort. Opt-in because a latency
assertion in the default suite becomes a flaky test that gets deleted.

---

## 12. Cuts

Blunt, with reasons.

| Cut | Why |
|---|---|
| Custom fields | ROADMAP §7. It is the feature that turns a query layer into a schema-migration engine. |
| Per-project configurable workflows | States are renamable within five fixed categories. Every downstream rule keys off the category, so "configurable" would mean configurable semantics, which is Jira. |
| Per-project permission schemes | A project is readable by every workspace member. Private projects are one `acl_grant` once plan 00 lands (§2a) — not a second permission model now. |
| Issue attachments | `files` are only reachable through their message (`internal/search/doc.go:305-310`). An issue-owned file has no ACL until Drive exists. Link the message instead. |
| Reactions on comments | Would require dropping the `reactions.message_id` FK on a hot shipped table, for an emoji (§4). |
| Multi-level sub-issues | One level. Enforced by the parent predicate, not a CHECK. Trees invite tree queries, tree reordering and tree permissions. |
| Bulk edit | Cheap to build, expensive in events, activity rows and notification volume. First thing to add after v1, deliberately not in it. |
| Time tracking, burndown, velocity | ROADMAP §5. Estimates are stored; nothing is computed from them yet. |
| Timeline/Gantt, milestones, roadmaps | A third view type and a second hierarchy. Not until list and board are good. |
| Issue templates, triage inbox, SLAs | Process features for organizations that have outgrown this product's target. |
| Postgres full-text on issues | Meilisearch already indexes them with an ACL filter. Two search paths is two answers to "why didn't this match". |
| Import from Jira/Linear | A product in itself, and it collides with `issue_counter` serialization. |

---

## 13. Sizing

| Piece | Size |
|---|---|
| Migration 017 + issue model/repository | M |
| `rank.go` + move + renormalizer | S code, **L in consequence** — everything ordering-related fails through it |
| `query.go` + `filter.go` + board/list/count | **L — the long pole** |
| `internal/project` (states, labels, cycles) | M |
| `internal/comment` + migration 018 | M |
| Events + search doc + reindex source + worker durables | M |
| Notifications + inbox object reference | S |
| Seams (message→issue, preview, huddle check, `ws` project subscription) | M total, S each |
| Saved views | S |
| Client: list + filters + detail | M |
| Client: board + drag | **L** |

Overall **M–L**, matching ROADMAP §5. The long pole is the board end to end. The
piece most likely to be underestimated is not the board — it is `filter.go`,
because it looks like plumbing and is the phase's entire injection surface.
