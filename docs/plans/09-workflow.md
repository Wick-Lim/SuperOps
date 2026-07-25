# Plan 09 — Workflow

**Phase 7. Last, and deliberately narrow** (ROADMAP §6). It is only valuable once
there is something to automate, and it triggers on everything above it.

Status: design. Not started. Depends on Phase 0 (`00-permissions.md`) for the
subject/object check every action needs, and on each pillar it triggers on.

---

## 1. What it is

A workflow is a saved rule: *when this happens in SuperOps, do these things in
SuperOps*. A member builds it from a trigger (a message posted in `#alerts`, an
issue moved to Done, a file dropped in a folder, mail arriving in `support@`)
and an ordered list of action nodes, with `on error` branches and a condition
node for the one place a rule needs to fork. One node type reaches outside: an
authenticated HTTP request, with its credential held in a workspace vault the
workflow author can reference but never read.

Every run is recorded: which event started it, what each step received, what it
returned, how long it took, why it stopped. When a trigger fires and the
workflow does *not* run, that is recorded too, with the reason — because the
common failure of every automation product is a rule that silently does
nothing.

**Not in scope for v1:**

- **Arbitrary code execution.** ROADMAP §7. §9 of this plan is the whole
  argument, including what would have to be true to revisit it.
- **n8n connector parity.** ROADMAP §7. The HTTP node plus the credential vault
  is the general answer; a "connector" is a preset over it, and 400 presets is a
  maintenance treadmill, not engineering.
- **Parallel branches and joins.** The graph is a tree: sequential with
  branching, no fan-out, no join. §4 explains what this buys, including the
  editor.
- **Loops / `foreach`.** Unbounded run length. First follow-on, bounded.
- **An expression language.** Structured predicates and path-only templating
  (§5). A string expression evaluator is code execution with better marketing.
- **Sub-workflows, human-in-the-loop approval steps, a template gallery.**
- **A realtime run feed.** The run screen polls. See the cut table (§13).

---

## 2. What already exists, and what it actually buys

| Asset | What workflow takes from it | Where |
|---|---|---|
| JetStream durable consumers with ack / nak-with-backoff / term | The entire ack decision for a step, unchanged. `bindDurable`'s three-way switch *is* the step engine's outer loop. | `cmd/worker/main.go:527`, `:568` |
| `permanentError` structural interface | "This step can never succeed" vs "the provider is down" — already the codebase's retry vocabulary, matched structurally so no package has to import another's error types. | `cmd/worker/main.go:482` |
| Periodic job loop + `withSingletonLock` | The reaper and the scheduler are two more `start(name, delay, interval, fn)` calls with an advisory lock. No new scaffolding. | `cmd/worker/main.go:261`, `:673` |
| `promoteScheduled`'s due-row pattern | `FOR UPDATE SKIP LOCKED` over a `<= NOW()` predicate, then publish. The schedule trigger and the resume path are this query with a different table. | `cmd/worker/main.go:767` |
| `internal/authz` | Who a run acts as, and whether it may still read the object that triggered it. | `internal/authz/authz.go:214`, `:232` |
| `internal/sso`'s AES-256-GCM secret handling | The credential vault, verbatim: nonce-prefixed seal, key parsed in three operator-friendly encodings, a decrypt error that names the likely cause. | `internal/sso/secret.go:24`, `:56`, `:77` |
| `internal/mail`'s publisher/consumer split | The template for "the API records intent, the worker does the work". Also literally the send-mail action: `Publisher.Queue` is one call. | `internal/mail/queue.go:90` |
| `internal/webhook`'s post path | The post-message action, including posting as a real user rather than a fictional root account, and sanitizing the display name. | `internal/webhook/handler.go:380`, `:423` |
| Keyset pagination | Run history is `(created_at, id)` keyset like every other list. | `pkg/httputil/pagination.go:22` |
| `{data, meta, error}` envelope, `AppError` | Unchanged. | `pkg/httputil/response.go:19`, `errors.go:8` |

**No new Go dependencies.** Everything above is stdlib plus what is already in
`backend/go.mod`. Two temptations are refused explicitly: `robfig/cron` (see the
schedule cut, §13) and any expression-language package (`expr-lang/expr`,
`antonmedv/expr`) — an embedded evaluator is exactly the surface §9 exists to
avoid, and buying it as a dependency does not change that.

### Three facts about the existing NATS setup that shape everything

These are not incidental; each one changes a design decision.

**(a) The shared stream is `InterestPolicy`.** `EnsureEventStream` creates
`SUPEROPS` over `superops.>` with `Retention: jetstream.InterestPolicy`
(`internal/app/app.go:440-448`). Under interest retention a message is stored
*only* if some consumer's filter matches it at publish time. Today
`presence.changed` and `unread.update` match nothing and are discarded on
arrival. **A workflow trigger consumer bound to `superops.>` would start
persisting every presence transition in the deployment to file storage.** The
trigger consumer therefore uses `FilterSubjects` (plural — supported in
nats.go v1.49, `jetstream/consumer_config.go:236`) with an explicit allowlist
built from the trigger catalog, and adding a trigger type is a change to that
list. This is a startup assertion, not a comment.

**(b) Interest retention also means the consumer must exist before the event.**
A workflow created at 10:00 cannot see events published at 09:59; they were
discarded. So the trigger consumer is bound unconditionally at worker boot —
like the mailer (`cmd/worker/main.go:251`) — not lazily when the first workflow
is created. It also must never be deleted while workflows exist: deleting it
opens a hole with no error and no backfill path.

**(c) Some domain events are published with core NATS, not JetStream.**
`Hub.publishDomain` uses `natsConn.Publish` (`internal/ws/hub.go:388`) — no
storage ack, no `Nats-Msg-Id`, silently lost if NATS blinks. So does the
incoming-webhook message publish (`internal/webhook/handler.go:417`), which
means a message posted by a webhook would trigger workflows *unreliably*, while
the same message posted through `POST /messages` triggers them reliably
(`internal/message/handler.go:113` uses `PublishDurable`). **Any event promoted
to a trigger must first move to `PublishDurable` with a dedupe id.** That is a
small, independently shippable prerequisite (§11) and it is not optional: a
workflow built on a lossy event is not debuggable, because the missing run and
the never-published event look identical.

---

## 3. Data model

**Migration numbers.** 000–012 exist, 013 is an unused hole, 014 (SSO) and 015
(collab) are in flight, 016 is claimed by unified search, and 017/018 are
contested between Drive, work tracking and the Phase-0 remainder. This phase
ships last, so it takes **the next two free numbers when the branch is cut;
nominally 024 (engine) and 025 (credential vault)**. Do not reclaim 013:
`golang-migrate` stores a single version integer, so a lower-numbered file added
later is skipped on every already-migrated database, silently.

### 024 — definitions, triggers, runs

```sql
CREATE TABLE workflows (
    id              UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id    UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name            TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 200),
    description     TEXT NOT NULL DEFAULT '',

    -- The subject every action in every run authorizes as. NOT "whoever last
    -- clicked save": an editor with write access to the workflow must not be
    -- able to escalate to the owner's channel access by editing a node.
    -- RESTRICT, not SET NULL: a workflow with no subject would either stop
    -- working or run unauthenticated, and the second is unthinkable.
    owner_id        UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    enabled         BOOLEAN NOT NULL DEFAULT FALSE,
    -- Why the engine turned it off: 'consecutive_failures', 'owner_inactive',
    -- 'owner_lost_access', 'invalid_graph'. Rendered verbatim in the UI. A
    -- workflow that stopped working and does not say so is the failure mode
    -- this whole plan is organised around.
    disabled_reason TEXT,
    disabled_at     TIMESTAMPTZ,

    live_version    INT,
    created_by      UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX idx_workflows_workspace ON workflows (workspace_id, created_at DESC, id DESC);

-- Immutable versions. A run pins one, so editing a workflow can never change
-- the graph under a run that is mid-flight — and the run history stays
-- interpretable, because the graph it executed still exists.
CREATE TABLE workflow_versions (
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INT  NOT NULL CHECK (version > 0),
    graph       JSONB NOT NULL,     -- nodes, edges, trigger; validated on write
    created_by  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workflow_id, version)
);

-- The trigger, denormalized out of graph so dispatch is one indexed query per
-- event rather than "load and parse every workflow in the workspace".
CREATE TABLE workflow_triggers (
    workflow_id  UUID PRIMARY KEY REFERENCES workflows(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version      INT  NOT NULL,

    -- 'message.created', 'issue.state_changed', 'schedule', 'manual', ...
    -- TEXT + shape CHECK, not an enum: registering a trigger when a pillar
    -- ships must not need a migration. Same reasoning as
    -- collab_documents.resource_type (migrations/015_collab.up.sql).
    event_type   TEXT NOT NULL CHECK (event_type ~ '^[a-z][a-z0-9_]*\.[a-z][a-z0-9_]*$'),

    -- The object the rule is scoped to (a channel, a project, a folder), or
    -- NULL for "anywhere in the workspace". Evaluated in SQL, before the graph
    -- is loaded.
    scope_type   TEXT,
    scope_id     UUID,
    -- Structured predicates, ANDed. See §5 — deliberately not an expression.
    predicate    JSONB NOT NULL DEFAULT '[]'::jsonb,

    -- Schedule triggers only.
    schedule     JSONB,
    next_run_at  TIMESTAMPTZ,

    enabled      BOOLEAN NOT NULL DEFAULT FALSE
);
CREATE INDEX idx_workflow_triggers_dispatch
    ON workflow_triggers (workspace_id, event_type, scope_id) WHERE enabled;
CREATE INDEX idx_workflow_triggers_due
    ON workflow_triggers (next_run_at) WHERE enabled AND next_run_at IS NOT NULL;

CREATE TABLE workflow_runs (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id   UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    version       INT  NOT NULL,

    -- Copied from workflows.owner_id when the run starts, so a mid-run
    -- ownership change cannot retroactively widen what the remaining steps do.
    actor_id      UUID NOT NULL REFERENCES users(id) ON DELETE RESTRICT,

    status        TEXT NOT NULL CHECK (status IN
                    ('pending','running','waiting','succeeded','failed','cancelled','timed_out')),
    mode          TEXT NOT NULL DEFAULT 'live' CHECK (mode IN ('live','dry_run')),

    -- Identifies the event that started this run. UNIQUE with (workflow_id,
    -- version) below: this is what makes trigger dispatch safely retryable —
    -- a redelivered trigger event finds the existing run instead of starting a
    -- second one.
    trigger_key     TEXT  NOT NULL,
    trigger_payload JSONB NOT NULL,

    -- Loop control (§10). depth is inherited when the triggering object was
    -- itself produced by a workflow.
    parent_run_id UUID REFERENCES workflow_runs(id) ON DELETE SET NULL,
    root_run_id   UUID,
    depth         INT NOT NULL DEFAULT 0 CHECK (depth BETWEEN 0 AND 8),

    -- Resume state. cursor_node is the node the next step message will execute;
    -- lease_expires_at is what the reaper looks at.
    cursor_node      TEXT,
    resume_at        TIMESTAMPTZ,
    lease_expires_at TIMESTAMPTZ,
    steps_used       INT NOT NULL DEFAULT 0,

    error_code    TEXT,
    error_message TEXT,
    started_at    TIMESTAMPTZ,
    finished_at   TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_runs_trigger_unique UNIQUE (workflow_id, version, trigger_key)
);
CREATE INDEX idx_workflow_runs_list ON workflow_runs (workflow_id, created_at DESC, id DESC);
CREATE INDEX idx_workflow_runs_resumable
    ON workflow_runs (COALESCE(resume_at, lease_expires_at))
    WHERE status IN ('pending','running','waiting');

CREATE TABLE workflow_step_runs (
    run_id        UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    node_id       TEXT NOT NULL,
    attempt       INT  NOT NULL DEFAULT 1,
    seq           INT  NOT NULL,           -- execution order, for display
    node_type     TEXT NOT NULL,
    status        TEXT NOT NULL CHECK (status IN ('running','succeeded','failed','skipped')),
    -- Capped and truncated by the writer; an HTTP response is not bounded and
    -- run history must not become the largest table in the database.
    input         JSONB,
    output        JSONB,
    truncated     BOOLEAN NOT NULL DEFAULT FALSE,
    error_code    TEXT,
    error_message TEXT,
    started_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    finished_at   TIMESTAMPTZ,
    PRIMARY KEY (run_id, node_id, attempt)
);

-- Why a trigger fired and did NOT produce a run. A bounded ring per workflow,
-- trimmed by the GC job. This is the single highest-value debugging feature in
-- the plan: without it "my workflow does nothing" is unanswerable.
CREATE TABLE workflow_trigger_rejections (
    workflow_id     UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    id              BIGSERIAL,
    event_subject   TEXT NOT NULL,
    event_summary   JSONB NOT NULL,
    -- 'predicate' | 'owner_no_access' | 'owner_inactive' | 'loop_guard'
    -- | 'concurrency_cap' | 'rate_limited' | 'disabled' | 'scope_mismatch'
    reason          TEXT NOT NULL,
    predicate_index INT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (workflow_id, id)
);

-- Provenance for loop detection (§10). Short-lived; purged by the GC job.
CREATE TABLE workflow_effects (
    object_type TEXT NOT NULL,
    object_id   UUID NOT NULL,
    run_id      UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (object_type, object_id)
);
CREATE INDEX idx_workflow_effects_gc ON workflow_effects (created_at);
```

`updated_at` gets the trigger migration 009 group F installs for every other
table; do not hand-roll it.

### 025 — the credential vault

```sql
CREATE TABLE workflow_credentials (
    id            UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id  UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name          TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 100),
    kind          TEXT NOT NULL CHECK (kind IN ('bearer','basic','header','query')),

    -- AES-256-GCM, nonce || sealed, keyed by WORKFLOW_SECRET_KEY. Identical
    -- shape to sso_providers.client_secret_enc (migrations/014_sso.up.sql), and
    -- for the identical reason: this is a symmetric credential we must replay
    -- to a third party, so it cannot be hashed. No read path that feeds an API
    -- response ever selects this column.
    secret_enc    BYTEA NOT NULL,
    -- The non-secret half: header name, basic-auth username, query parameter.
    public_part   TEXT NOT NULL DEFAULT '',

    -- THE containment control. A credential may only be attached to a request
    -- whose (post-DNS, post-redirect) host matches this list. Set by a
    -- workspace admin when the credential is created; a workflow author who may
    -- *reference* the credential cannot widen it, so they cannot point it at a
    -- server they control and read it out of their own access log.
    allowed_hosts TEXT[] NOT NULL CHECK (cardinality(allowed_hosts) BETWEEN 1 AND 20),

    created_by    UUID REFERENCES users(id) ON DELETE SET NULL,
    last_used_at  TIMESTAMPTZ,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (workspace_id, name)
);
```

---

## 4. The execution model, and exactly what a step guarantees

### The mapping onto JetStream

**A separate stream, not `SUPEROPS`.**

```
Name:       SUPEROPS_WF
Subjects:   ["wf.step"]
Retention:  WorkQueuePolicy     -- a step message is work, not a fact; it is
                                -- removed on ack rather than kept for an
                                -- audience
Storage:    FileStorage
MaxAge:     7 days
Duplicates: 2 minutes           -- matches PublishDurable's Nats-Msg-Id use
MaxMsgs / Discard: bounded, DiscardNew
```

Three reasons it is not the shared stream. Interest retention is wrong for a
work queue. `MaxAge: 24h` (`internal/app/app.go:445`) is wrong for a run that
retries with backoff. And a runaway workflow filling the shared stream would
take message indexing and notifications down with it — a separate stream makes
the blast radius the workflow engine and nothing else.

**One JetStream message per step, not per run.** A run is Postgres state; a step
message is a token that says "wake up and execute node N of run R, attempt A".
Running a whole workflow inside one consumer callback is the obvious design and
it is wrong: `handlerTimeout` is `consumerAckWait - 5s` = 25 s
(`cmd/worker/main.go:90-96`), so any workflow containing one slow HTTP node
exceeds it, the server hands a second copy to another replica, and now two
replicas are executing the same run.

The step consumer therefore needs different constants from the four existing
durables — `AckWait: 90s`, a matching handler timeout, `MaxDeliver: 3`. That is
a new optional field on `durableSpec` (`cmd/worker/main.go:472`), not a fork of
`bindDurable`; the ack switch at `:568` is exactly right as it stands.

**Retry is application state, not redelivery.** JetStream redelivery means only
one thing here: *the worker died holding this step*. A node's own retry policy
(3 attempts, exponential backoff) is implemented by committing
`status='waiting', resume_at=NOW()+backoff` and acking; the scheduler job
re-publishes when it is due. That keeps a five-minute backoff from occupying a
`MaxAckPending` slot for five minutes, and it makes the retry visible in the run
history as a numbered attempt instead of invisible in a NATS counter.

### One step, in order

```
1. Fetch step message {run_id, node_id, attempt}.
2. BEGIN
   2a. SELECT ... FROM workflow_runs WHERE id = $1 FOR UPDATE
       - status in (cancelled, succeeded, failed) -> COMMIT, Ack, done
       - lease_expires_at > NOW() and held elsewhere -> ROLLBACK, Nak(delay)
       - steps_used >= cap -> mark timed_out, COMMIT, Ack
   2b. If a workflow_step_runs row for (run_id, node_id, attempt) is already
       'succeeded' -> skip execution, go to 5. THIS IS THE IDEMPOTENCE HINGE.
   2c. INSERT the step row as 'running'; set the run's lease.
   COMMIT
3. Execute the node with a bounded context (default 30 s, max 60 s), as
   run.actor_id, through internal/authz.
4. BEGIN
   4a. UPDATE the step row: status, output (capped), error, finished_at.
   4b. Compute the next node from the pinned graph; UPDATE the run's
       cursor_node / status / resume_at; steps_used += 1.
   COMMIT
5. Publish the next step message with Nats-Msg-Id = "<run_id>:<node>:<attempt>".
6. Ack.
```

### The guarantee, stated precisely

> **The step message is delivered at least once. The step's *result* is
> committed at most once. The step's *side effect* is effectively-once for
> internal actions and at-least-once for the HTTP node.**

The middle clause is what step 2b buys: a redelivered message whose attempt
already committed as `succeeded` does not re-execute, it re-derives the next
step and re-publishes. The last clause is the honest part. Internal actions
carry an idempotency key `run_id:node_id:attempt`, which is either the
`Nats-Msg-Id` of the event they publish or a value written into a UNIQUE
`origin_key` column on the object they create, so a re-execution collides and
reads back the row it made the first time. An outbound HTTP POST has no such
handle — the request may have been received and the response lost. The HTTP node
therefore exposes an explicit `on_crash` setting: `retry` (at-least-once,
default) or `fail` (at-most-once). Choosing is the author's job and the editor
says so in those words.

### What happens when the worker restarts mid-run

Four cases, all of which must be reasoned about, and only three of which
JetStream covers:

1. **Crash before the result commits (step 4).** Nothing was recorded. `AckWait`
   expires at 90 s, the message is redelivered, the step runs again from its
   committed input. Side effect repeats per the clause above.
2. **Crash after the commit, before the publish (step 5).** The step row says
   `succeeded`; no message exists for the next node. The *same* message was
   never acked, so it is redelivered, hits the 2b skip branch, and re-publishes.
   This is why the ack is last and why the publish is keyed — reverse them and
   this case becomes a permanently stalled run.
3. **Crash after the publish, before the ack.** Redelivered; 2b skip; the
   re-publish collapses inside the 2-minute duplicate window; ack.
4. **The message is gone.** Naked past `MaxDeliver`, a stream purge, an operator
   deleting the consumer, a `DiscardNew` rejection. The run sits in `running`
   forever with no error and nothing to redeliver. **JetStream cannot cover this
   and a step engine that ignores it has a silent-stall failure mode.** This is
   the `workflow_reaper` job: same `start(name, delay, interval, fn)` +
   `withSingletonLock` shape as retention (`cmd/worker/main.go:280`, `:673`),
   every 60 s, which finds runs in `pending`/`running` whose `lease_expires_at`
   has passed, re-publishes their `cursor_node`, and fails runs past their
   wall-clock budget with `error_code = 'run_timeout'`.

The reaper is not defensive padding. It is the only component that can turn "the
queue lost your run" into a visible failure, and it is the difference between a
step engine and a step engine you can operate.

---

## 5. Triggers, predicates and templating

### Dispatch

One durable, `workflow-trigger`, bound at worker boot on `SUPEROPS` with
`FilterSubjects` = the allowlist from §2(a). Per event:

1. Parse the workspace id out of the subject — the existing helper shape
   (`internal/channel/unread.go:210`, `internal/notification/service.go:715`);
   an empty workspace id is a permanent error, not a retry.
2. `SELECT ... FROM workflow_triggers WHERE workspace_id = $1 AND event_type = $2
   AND (scope_id IS NULL OR scope_id = $3) AND enabled` — one indexed query,
   never one per workflow.
3. For each candidate, in order: predicate → **owner read check** → loop guard →
   concurrency cap. Every rejection writes a `workflow_trigger_rejections` row
   naming which stage rejected it and, for a predicate, which one.
4. Insert the run (`trigger_key` = the event's dedupe id) and publish step 1.
   `ON CONFLICT DO NOTHING` on `workflow_runs_trigger_unique` makes a redelivered
   trigger event idempotent; the handler then re-publishes the pinned
   `cursor_node` rather than starting a second run.

**The owner read check is not optional and it happens at fire time, not at save
time.** Without it a workflow is a data-exfiltration primitive: trigger on every
message in the workspace, HTTP POST the content out. The check is
`CanReadChannel(ctx, ch, run.actor_id)` today
(`internal/authz/authz.go:214`) and `Capability(owner, obj, read)` after plan 00.
When it fails, the rejection reason is `owner_no_access`, and N consecutive
failures auto-disable the workflow with that reason on the row.

### The trigger surface, by pillar

| Pillar | Triggers | State |
|---|---|---|
| Chat | `message.created`, `message.updated`, `message.deleted`, `reaction.added`, `channel.member_added` | Published durably today (`internal/message/handler.go:113`, `internal/channel/events.go:227`). Ready. |
| Chat (hub-published) | `channel.created`, `channel.updated`, `member_joined`, `member_left` | Core publish only (`internal/ws/hub.go:388`). **Needs the §2(c) promotion first.** |
| Drive | `file.created`, `file.updated`, `file.trashed`, `folder.shared` | Subject shape already assumed by the in-flight search indexer (`internal/search/indexer.go:23`). |
| Work tracking | `issue.created`, `issue.state_changed`, `issue.assigned`, `comment.created` | The richest triggers of the eight, per ROADMAP §5. |
| Email | `mail.received` | The highest-value single trigger: support mail → issue. |
| Huddle | `huddle.started`, `huddle.ended` | Per `07-huddle.md`. |
| Docs / Sheet / Design | **none** | Deliberate cut. Editors emit CRDT updates, not domain facts; a per-keystroke trigger is nonsense and "a doc was created" is a Drive event. |
| Workflow | `workflow.run_failed` | So alerting is itself a workflow. Depth-guarded like anything else. |
| — | `manual`, `schedule` | Manual is essential for testing. Schedule is §13's structured form. |

### Predicates and templating: structured, not expressive

A predicate is `{"path": "$.message.content", "op": "contains", "value": "deploy"}`
— `eq`, `neq`, `contains`, `starts_with`, `matches` (RE2, length-capped),
`in`, `gt`, `lt`, `exists`. ANDed at the trigger; the condition node adds one
level of OR grouping. Templating in action inputs is path substitution only:
`{{ $.trigger.message.content }}`, `{{ $.node_3.body.id }}`. No functions, no
arithmetic, no concatenation beyond literal text around a substitution.

This is the same decision as §9, applied at a smaller scale. A string expression
evaluator is a language runtime with a friendlier name, and once it exists
someone will ask for a loop in it. The cost of the restriction is real — you
cannot uppercase a string in v1 — and the escalation for it is a first-class
transform node (§9, step 2), not an expression grammar.

**Templating can never reference a credential.** The credential is resolved by
the HTTP node's executor, at execution time, into the request's auth position
only. If `{{ $.credentials.stripe }}` resolved to anything, the first workflow
someone writes puts it in a chat message.

---

## 6. Actions, and who they run as

| Action | Reuses |
|---|---|
| `chat.post_message` | The webhook path verbatim: insert + `last_message_at` bump + publish (`internal/webhook/handler.go:388-408`), authored by `run.actor_id`, `content_type='system'`, name sanitized (`:423`) |
| `chat.add_reaction`, `chat.create_channel`, `chat.invite_member` | `internal/channel`, `internal/message` repositories |
| `drive.create_folder`, `drive.move`, `drive.share` | `internal/file` + Drive (Phase 1) |
| `issue.create`, `issue.transition`, `issue.assign`, `issue.comment` | Work tracking (Phase 2) |
| `mail.send` | `mail.Publisher.Queue` (`internal/mail/queue.go:90`) — one call, and it inherits the queue, the retry and the transport choice |
| `notify.user` | `internal/notification` |
| `http.request` | §7 |
| `control.condition`, `control.stop` | The engine itself |

**Every action authorizes as `run.actor_id` through `internal/authz`.** A
workflow is not a privileged actor; it is a person's rule executing with that
person's access. Three consequences worth stating because they read as bugs
otherwise:

- When the owner loses access to a target, the step fails with `FORBIDDEN` and
  the run history shows it. That is correct and it is *visible*, which is the
  whole point.
- When the owner is deactivated, every workflow they own auto-disables with
  `disabled_reason = 'owner_inactive'`. Plan 00's audit question — "this person
  left the company, what did they have access to?" — has a wrong answer if their
  rules keep running nightly.
- **Gap against plan 00.** Its subject model is "a user today; a group later; a
  share-link token eventually". A workflow is a fourth kind: a service principal
  acting on behalf of a user. v1 does *not* add one — the subject is the owner,
  and the run row records both `actor_id` and `workflow_id` so the audit trail
  names the rule as well as the person. If a later phase wants workflows to hold
  their own grants, `subject_type = 'workflow'` is the extension point; flagging
  it here rather than inventing it.

`workflow_effects` is written in the same transaction as any action that creates
an object, and every run's actions are audited through `audit.Service.Try`
(`internal/audit/service.go:73`) with `actor_id` and the run id in metadata.

---

## 7. The HTTP node and the credential vault

The HTTP node is the escape valve that makes cutting code execution tolerable:
anyone who genuinely needs logic writes a small service and calls it. For a
self-hosted product that is a *feature* — the customer's code runs on the
customer's compute with the customer's blast radius.

It is also the second-largest security surface in the phase, because it is an
authenticated outbound request originating **inside the deployment's network**,
where Postgres, Redis, NATS, MinIO and Meilisearch all live and most of them are
unauthenticated on the internal network under the shipped compose defaults.

### The egress guard

A dedicated `*http.Client` with its own `Transport`, not `http.DefaultClient`:

- Scheme allowlist: `http`, `https` only.
- **IP checked at dial time via `net.Dialer.Control`**, not by parsing the URL
  and resolving separately. Resolve-then-connect is a DNS rebinding hole: the
  name resolves to a public address for the check and to `127.0.0.1` for the
  connection. The `Control` hook sees the actual socket address.
- Deny by default: loopback, RFC1918, link-local (including `169.254.169.254`,
  the cloud metadata endpoint), unique-local IPv6, `0.0.0.0/8`, and
  IPv4-mapped-IPv6 forms of all of them. `WORKFLOW_HTTP_ALLOW_PRIVATE=false` is
  the default; an operator who genuinely wants internal calls flips it
  knowingly, and it is validated at boot like every other capability in
  ROADMAP §3c (`internal/app/config.go:510` is the pattern).
- Redirects: capped at 3, and **every hop re-runs the guard and the credential's
  `allowed_hosts` check** — a 302 to a different host must not carry the
  Authorization header.
- Response body capped (256 KiB) via `io.LimitReader`; stored output capped
  lower still, with `truncated = true`.
- Per-workspace outbound concurrency cap, so 500 runs pointed at a server that
  accepts and never answers cannot exhaust the worker's file descriptors.

### The vault

`pkg/crypto/aead.go` holds `ParseAESKey` / `Seal` / `Open`, lifted verbatim from
`internal/sso/secret.go:24-112` — including the three-encoding key parser and
the decrypt error that says "has the key changed?", which is the difference
between a five-minute diagnosis and a day of one. `internal/sso` collapses onto
it in a follow-up; do not touch that file while the SSO branch is in flight.

Rules, all of them load-bearing:

- Keyed by `WORKFLOW_SECRET_KEY`, separate from `SSO_SECRET_KEY`. Sharing one
  key means rotating either forces rotating both.
- **No API read path ever selects `secret_enc`.** Create and rotate accept a
  plaintext; nothing returns one. Same contract as webhook tokens
  (`internal/webhook/handler.go:175`).
- Only workspace admins create or edit credentials, including `allowed_hosts`.
  Any workspace member may *reference* one by id in a node. That split is the
  containment: reference without read, and a host binding the referencer cannot
  change.
- Step output for an HTTP node stores headers with the credential's value
  replaced by `***`, and a scrubber runs over the whole stored request/response
  comparing against the resolved secret — an API that echoes your key back in an
  error body must not put it in the run log.
- `last_used_at` is maintained, so an admin can find credentials nothing uses.

---

## 8. Run history, debugging, and the API surface

The design premise: **the interesting state is the run that did not happen.**

- `GET /workflows/{id}/rejections` — why triggers fired without running,
  including which predicate rejected them. Bounded ring, trimmed by the GC job.
- `POST /workflows/{id}/runs` with `{"mode": "dry_run", "payload": …}` — run the
  live graph against a synthetic payload or a replay of a past run's trigger,
  with every side-effecting node reporting what it *would* do (including the
  fully rendered HTTP request, credential redacted) instead of doing it. This is
  the feature that makes the editor usable; build it with the engine, not after.
- `POST /runs/{run_id}/retry` — re-run from the first failed node, as a new run
  with `parent_run_id` set, against the version the original pinned.
- Per-step input/output/error/duration, and a failing run raises a
  `workflow.run_failed` event, which is both a notification and a trigger.
- Auto-disable after N consecutive failed runs, with `disabled_reason` on the
  row and a notification to the owner. A workflow that has been broken for three
  weeks should say so on its own card.

### Routes

Conventions unchanged: `RegisterRoutes(mux, authMw)`, `{data,meta,error}`,
keyset cursors on every list. Note the ServeMux pattern-conflict lesson from
`internal/webhook/handler.go:47-53` — the two id spaces (`/workflows/{id}` and
`/runs/{id}`) are kept disjoint deliberately.

```
POST   /api/v1/workspaces/{workspace_id}/workflows
GET    /api/v1/workspaces/{workspace_id}/workflows                 keyset
GET    /api/v1/workspaces/{workspace_id}/workflow/catalog
GET    /api/v1/workflows/{workflow_id}
PATCH  /api/v1/workflows/{workflow_id}                             name, description, enabled
PUT    /api/v1/workflows/{workflow_id}/graph                       new version; 409 on stale base_version
DELETE /api/v1/workflows/{workflow_id}
GET    /api/v1/workflows/{workflow_id}/runs                        keyset (created_at, id)
POST   /api/v1/workflows/{workflow_id}/runs                        manual / dry-run
GET    /api/v1/workflows/{workflow_id}/rejections                  keyset
GET    /api/v1/runs/{run_id}
GET    /api/v1/runs/{run_id}/steps
POST   /api/v1/runs/{run_id}/cancel
POST   /api/v1/runs/{run_id}/retry

POST   /api/v1/workspaces/{workspace_id}/workflow/credentials      admin
GET    /api/v1/workspaces/{workspace_id}/workflow/credentials      admin; never returns a secret
PATCH  /api/v1/workflow/credentials/{credential_id}                admin; rotate / edit hosts
DELETE /api/v1/workflow/credentials/{credential_id}                admin
```

`/catalog` is computed from what `app.New` actually constructed, so a deployment
with `FILES_ENABLED=false` does not offer Drive actions and a deployment with
`MAIL_TRANSPORT=log` marks `mail.send` as non-delivering. ROADMAP §3c says
capabilities are deployment properties; the catalog is where that becomes
visible to the person building a rule, instead of at run time in a failed step.

---

## 9. The hard part — the one v1 buys its way out of

**Running user-supplied code safely.** ROADMAP §7 cuts it. This section exists
so the cut is revisitable rather than forgotten, which means writing down what
it would actually cost and what would have to change for the answer to flip.

### Why it is cut

The threat model is unusually bad here, and not for the usual multi-tenant
reason. A workflow author is any workspace member. Their code would execute
inside the worker's network, next to Postgres, MinIO, NATS, Meilisearch and
Redis — most of which the shipped compose and Helm defaults leave
unauthenticated on the internal network, because the only thing that reaches
them is our own binary. A sandbox escape is therefore not "a tenant read another
tenant's row"; it is "the attacker has the database". And in a self-hosted
product the operator *is* the victim, with no security team between the CVE and
the outage.

The value on the other side is smaller than it looks. Most requests for a code
node are requests to reshape a payload, and most of the rest are requests to
call something — which the HTTP node already does, on the customer's compute.

### What it would require

| Approach | What you get | What it costs |
|---|---|---|
| **`goja`** (pure-Go JS) | Easy to embed, no CGO | **Not a security sandbox.** No enforceable heap cap, CPU bounded only by an interrupt hook, and one bug in a pure-Go interpreter is a Go-heap bug in our process. Fine for trusted config, wrong for member-supplied code. |
| **V8 isolates** (CGO) | Real heap limits, real interrupts, per-isolate teardown | A CGO dependency, a monthly stream of V8 CVEs to track, and the isolate still shares a process with our Go heap. The right answer for a hosted product with a security team on call; not for an unattended self-hosted binary. |
| **gVisor (`runsc`)** | A genuine kernel boundary; ~10–50 ms warm start with a pool | Requires a container runtime under the worker, which contradicts "the deploy is compose + Helm and the worker is one Go binary". Absent on macOS and on several managed Kubernetes offerings without node-level configuration. |
| **Firecracker microVMs** | The strongest boundary available | A VMM, a kernel image, a rootfs pipeline, snapshotting, and KVM — so bare metal or nested-virt instances. This is a platform, not a feature. |
| **WASM via `wazero`** (pure Go) | Real fuel metering, real memory limits, no CGO, keeps the single-binary deploy | The user-facing language stops being JavaScript unless you also ship QuickJS compiled to WASM and accept its performance. **This is the option that has changed since ROADMAP §7 was written**, and it is the one I would reach for. |

And the sandbox is the part people remember. The rest, which is most of the
work: CPU/wall/memory quotas per run *and* per workspace; a network policy for
the sandbox — which is the same egress guard §7 already needs, so the HTTP node
is a prerequisite for code nodes rather than an alternative to them; a
filesystem story; a dependency story (say npm, and you have inherited a package
registry and a supply chain); a debugging story (stack traces, console output,
what a timeout looks like to the author); and a deprecation story, because you
can never remove a language feature once a customer's rule depends on it.

### The escalation path

Written as conditions, so it is a decision and not a wish:

1. **Trigger to revisit.** Three or more independent requests that cannot be
   expressed as (a) a new first-class action node, (b) a better predicate or
   path, or (c) an HTTP call to a service the customer runs. Until then the
   answer is "add a node", and adding a node is a day's work behind the catalog.
2. **First escalation — no sandbox required.** A **pure transform node**: a
   JMESPath/JSONata-shaped grammar, data in, data out, no I/O, no host access,
   bounded by an evaluation-step counter. This covers the overwhelming majority
   of what people want code for. It is a small interpreter over a fixed grammar
   written in Go — roughly 600 lines, exhaustively testable, no new dependency.
   **This is the escalation I expect to take, and it should be priced as M.**
3. **Second escalation — a real sandbox.** `wazero` + a QuickJS WASM build,
   running in a **separate process** (`cmd/wfsandbox`) that holds no database
   credentials, no NATS connection and no filesystem, with fuel and memory
   limits set at instantiation; the step executor talks to it over stdin/stdout
   with a bounded payload and a hard deadline. Two boundaries instead of one.
   Two engineer-months plus a permanent security surface, and it needs a written
   threat model and a named on-call owner before it starts — not a ticket.
4. **Never in the default deployment.** Containers, gVisor, Firecracker. If a
   customer needs that isolation level, it is a separate tier with a separate
   operational contract, priced accordingly.

The standing rule: ROADMAP §7's cut holds until step 1's condition is met on
paper, and step 3 does not begin without step 2 having shipped and failed to
satisfy the requests that triggered it.

### The hard parts that do not go away

Cutting code execution removes the worst risk; it does not make v1 easy. The
three that remain are §4's crash-resume semantics, §7's egress guard, and §10's
feedback loops. The first has a clean correctness argument, the second is where
a mistake is a security incident, and the third is the one most automation
products ship broken.

---

## 10. Feedback loops

A workflow that posts a message, triggering a workflow that posts a message, is
the classic automation footgun, and a per-run step cap does not catch it — each
run is short and well behaved; there are simply infinitely many of them. The
nastiest variant is mutual: A triggers B triggers A, with neither workflow
looking wrong on its own.

The mechanism is provenance on the *object*, not a counter on the run:

1. Any action that creates or modifies an object writes
   `workflow_effects (object_type, object_id, run_id)` in the same transaction.
2. When an event matches a trigger, the dispatcher looks up the event's primary
   object in `workflow_effects`. A hit means the event was manufactured by a
   workflow, so the new run inherits `parent_run_id`, `root_run_id` and
   `depth + 1`.
3. Refuse if `depth > 3` (configurable, hard cap 8), or if this `workflow_id`
   already appears in the ancestor chain. Refusal writes a rejection with reason
   `loop_guard`, so the author sees the cycle rather than a mystery.
4. `workflow_effects` is purged after an hour by the GC job — long enough for
   the chain, short enough that the table stays small.

Cleaner long-term: an `origin` field on `natspkg.Event` so provenance rides the
event itself. That touches every publisher in the codebase, which is why v1 does
it with a lookup table instead — but it is the right shape when a second
consumer needs the same information, and it should be reconsidered then.

Belt and braces on top: a per-workspace concurrent-run cap and a per-workflow
run-rate cap, both of which reject *visibly* (`concurrency_cap`,
`rate_limited`) rather than queueing silently.

---

## 11. Package layout

```
internal/workflow/
  model.go        graph schema, node registry, validation (cycles, unknown
                  node types, unreachable nodes, depth, referenced credentials)
  repository.go   Postgres; keyset lists
  handler.go      the routes in §8
  catalog.go      what this deployment can trigger on and do (§8)
  trigger.go      the dispatch consumer, predicates, rejection log
  engine.go       the step consumer: lease, execute, commit, publish, ack
  scheduler.go    due schedules + due resumes; the reaper
  template.go     path substitution
  credential.go   the vault: seal, open, allowed_hosts enforcement
  action/         one file per pillar; each is Execute(ctx, ActionCtx) (json.RawMessage, error)
  httpnode/       the HTTP node and the egress guard (its own package so the
                  guard is testable and reviewable in isolation)

pkg/crypto/aead.go   ParseAESKey / Seal / Open, lifted from internal/sso/secret.go

cmd/worker/          three binds (trigger, step, and the step consumer's own
                     constants) and two job loops (scheduler, reaper+GC)
```

Rebuilt: nothing. Reused: authz, audit, mail publisher, notification service,
message/channel/file repositories, the durable-consumer scaffolding, the job
loop, the advisory lock, keyset pagination, the envelope, the AES helper.

---

## 12. The editor

Web-first per ROADMAP §3; mobile reads runs and toggles a workflow on or off.

**The execution-model cut pays for the editor.** Because the graph is sequential
with branches and no joins, it is a *tree* — and a tree renders as an indented
list with branch columns. No canvas, no pan/zoom, no hit testing, no edge
routing, no `react-flow` (which is DOM-only and would break the one-codebase
bet, ROADMAP §3). That is the difference between an L client project and an M
one, and it is the single strongest argument for keeping joins out of v1.

- Node configuration forms are generated from the catalog's parameter schema, so
  adding an action is a backend-only change.
- Values are inserted from a **"insert reference" menu** populated from the
  outputs of preceding nodes — which are known statically from the graph — not
  typed into a free-text expression field. This is what makes path-only
  templating usable rather than annoying.
- The run viewer is the same tree with per-node status, timing and the
  input/output payloads, and it polls while a run is active.
- Cut: drag-to-connect, free layout, minimap, subgraph copy/paste, version diff.

---

## 13. Sequencing

**Prerequisite, independently shippable, do it first:** promote the
trigger-relevant publishes from core NATS to `PublishDurable` —
`Hub.publishDomain` for channel lifecycle events (`internal/ws/hub.go:388`) and
the incoming-webhook message publish (`internal/webhook/handler.go:417`, dedupe
id `message.new:<id>` to match `cmd/worker/main.go:886`). Small, valuable on its
own, and everything downstream is undebuggable without it.

**Ships first, blocks everything:** migration 024, `internal/workflow` model and
graph validation, the repository, and the step engine end-to-end with exactly
two node types — `trigger:manual` and `action:chat.post_message` — plus the
reaper. Narrow as possible, complete as possible. Everything after this is
width, not depth.

**Parallel from there:**

- Trigger dispatcher, predicates, rejection log, dry-run.
- The action library, one pillar at a time. Each is small and independent, and
  each blocks on its pillar existing.
- Credential vault + HTTP node + egress guard. **This is the long pole**, not the
  engine: the engine is careful work with a clean correctness argument, and the
  egress guard is where a mistake is a security incident. It needs a review that
  is not self-review, and it should start early because of that, not because of
  its size.
- The editor, from the day `/catalog` returns anything.

**Last:** schedule triggers, retry-from-failed-node, run retention, the
auto-disable notification.

---

## 14. Risks and failure modes

**Shared-stream coupling.** Adding an interest-holding consumer to `SUPEROPS`
changes when messages are freed: `message.created` now waits for the workflow
consumer's ack as well as the indexer's and the notifier's. A wedged workflow
consumer means the shared stream grows until `MaxAge` (24 h) — a new coupling
this phase introduces. Mitigations: `MaxDeliver` + `Term` (already the pattern),
and pending-count reporting per durable in the worker's `/health`
(`cmd/worker/main.go:1336` currently reports attempts, not backlog, so a wedged
consumer looks healthy).

**Thundering herd.** A workflow triggering on `message.created` in a busy
workspace turns every message into a Postgres transaction plus N steps. The
per-workspace concurrency cap rejects visibly rather than queueing; without it,
one enthusiastic rule degrades chat latency for everyone on the replica.

**Credential exfiltration.** Covered by four independent controls (host binding,
no templating access, redirect re-validation, output scrubbing) because any one
of them failing alone should not be sufficient.

**SSRF into our own infrastructure.** §7. The dial-time check is the part that
is easy to get subtly wrong; the test table (§15) is the deliverable that proves
it.

**Slow-loris upstreams.** Bounded by step deadline, response cap and outbound
concurrency cap.

**Owner offboarding.** Auto-disable on deactivation. Otherwise a departed
employee's access keeps executing.

**Storage growth.** Step input/output is JSONB and HTTP responses are unbounded;
capped at write with a `truncated` flag, run history retained 30 days by a job
that follows `runRetention`'s batched, locked, bounded shape
(`cmd/worker/main.go:902`) rather than one unbounded `DELETE`.

**Clock and DST.** Schedules are stored in a workspace timezone and computed
forward from the last fire; a "daily at 02:30" schedule on a DST-skip day must
fire once, not zero or twice. Worth a unit test with a real IANA zone.

**Version drift.** Runs pin `(workflow_id, version)`; a graph edit cannot change
what an in-flight run does or what a historical run means.

---

## 15. Verification

**Unit.** Graph validation (cycles, unknown node types, unreachable nodes,
missing credential, depth limits). Predicate evaluation, table-driven. Template
substitution including injection attempts (`{{ $.credentials.x }}`, nested
braces, a path escaping into another run's namespace). Credential seal/open
round trip plus the wrong-key error message. Schedule arithmetic across a DST
boundary.

**The egress guard gets its own hostile-URL table**, and it is the test that
matters most in this phase: `127.0.0.1`, `::1`, `::ffff:127.0.0.1`,
`169.254.169.254`, `0.0.0.0`, `10.x`, `192.168.x`, decimal-encoded
`http://2130706433/`, a hostname whose A record is `127.0.0.1`, a public host
that 302s to a private one, a `file://` scheme, and a redirect to a host outside
the credential's `allowed_hosts`.

**Engine unit tests with a fake action registry** drive the four crash cases in
§4 by injecting a failure at each commit/publish/ack boundary and asserting the
run completes exactly once.

**Integration** — `backend/test/integration/workflow_test.go`, against the real
Postgres/NATS the suite already stands up. The harness runs `app.New` only and
starts no worker (`harness_test.go:186`), so the engine must be bindable from a
test the same way `cmd/worker` binds it: one exported `workflow.Bind(deps)` used
by both, with `mail_test.go:38`'s ephemeral-consumer pattern as the precedent
for reaching the stream directly.

1. `TestWorkflowTriggersFromMessage` — trigger on `message.created` in channel A,
   action posts to channel B; assert the message lands, the run is `succeeded`,
   and two step rows exist with the right order.
2. `TestWorkflowResumesAfterCrash` — suppress the next-step publish, run the
   reaper, assert the run completes **and channel B has one message, not two**.
3. `TestWorkflowRespectsOwnerPermissions` — remove the owner from the trigger
   channel; assert no run, a rejection row with reason `owner_no_access`, and
   auto-disable after N. This is this phase's tenancy test and it belongs
   alongside `TestCrossTenantChannelAccess` in `tenancy_test.go`.
4. `TestWorkflowLoopGuard` — a rule whose action re-triggers itself stops at the
   depth cap with a bounded number of runs and a `loop_guard` rejection.
5. `TestHTTPNodeRefusesPrivateAddress` — an `httptest` server on 127.0.0.1 is
   refused with `EGRESS_BLOCKED`; allowed only with an engine constructed with
   `AllowPrivate` (a separate engine instance, not a mutation of the shared
   harness config).
6. `TestCredentialNeverLeavesTheAPI` — create, then `GET`, and assert no
   response field and no stored step output contains the plaintext.
7. `TestTriggerConsumerFilterIsAnAllowlist` — assert the durable's
   `FilterSubjects` is the catalog's list and does **not** match
   `superops.*.presence.changed`. This is the regression guard for §2(a); it is
   the kind of mistake that costs disk in production and nothing in test.

CI needs no new services: Postgres, Redis and NATS are already there, and every
HTTP-node test uses `httptest`.

---

## 16. Sizing

| Piece | Size |
|---|---|
| Migration 024/025, model, graph validation, repository | M |
| Step engine — consumer, lease, resume, reaper, dry-run | L |
| Trigger dispatcher, predicates, rejection log, loop guard | M |
| Action library across pillars (S each, ~8 of them) | M–L |
| **Credential vault + HTTP node + egress guard** | **L — long pole** |
| Editor: tree, catalog-driven forms, run viewer | L |
| Schedule triggers | S |
| Ops: health/backlog, caps, run retention, GC | S |
| Prerequisite: durable publishes for hub/webhook events | S |

**Overall L.** ROADMAP §5 prices internal-only automation at M; that was before
run history, the rejection log and the egress guard were on the page, and it
does not include the per-pillar action fan-out. Calling it L rather than
discovering it.

The long pole is the credential vault and the HTTP node — not because it is the
most code, but because it is the piece where a mistake is a security incident
and the review cannot be self-review.

---

## 17. Cuts

| Cut | Why |
|---|---|
| Arbitrary code execution | ROADMAP §7 and §9 above. Escalation path is written down and conditional. |
| n8n connector parity | ROADMAP §7. The HTTP node plus the vault is the general answer; connectors are presets, and 400 presets is a treadmill. |
| Parallel branches and joins | A join needs a counting protocol and turns the tree into a graph, which also costs the editor its cheap renderer (§12). Sequential + branch covers internal automation. |
| Loops / `foreach` | Unbounded run length. First follow-on, bounded at 100 items and sequential. |
| An expression language | Structured predicates and path-only templating (§5). A string evaluator is §9 at a smaller scale. |
| Cron strings | Structured schedules (interval / daily / weekly at HH:MM in a workspace timezone). Avoids a new dependency and the "what does `0 0 * * 6#2` do" support load. |
| Realtime run feed on the WS hub | Polling an open run is 20 lines. Fan-out raises "who may see this run's output", which is not worth answering for a progress bar. |
| Sub-workflows | Depth accounting across boundaries, cross-version pinning, a second recursion surface. |
| Human-in-the-loop approval nodes | Needs the unified inbox (Phase 0 remainder) and its own permission question. Strong second release. |
| Editor triggers (docs / sheet / design) | CRDT updates are not domain facts. "A doc was created" is a Drive event and is covered. |
| Template gallery / marketplace | A content problem. Three seeded examples in the empty state, no more. |

---

## 18. Open questions for a human

1. **Do the pillars accept an `origin_key` column?** Effectively-once internal
   actions (§4) need a UNIQUE idempotency column on the create paths of Drive
   and work tracking. If they refuse, the fallback is duplicate objects on a
   crash — which for "create issue" is much worse than it sounds. This should be
   agreed with those plans before either ships, not negotiated after.
2. **Is "the workflow acts as its owner" acceptable to plan 00**, or should
   `subject_type = 'workflow'` exist so a rule can hold its own grants? v1
   assumes the former (§6); the latter is a cleaner audit story and a larger
   permission-model change.
3. **`WORKFLOW_HTTP_ALLOW_PRIVATE` default per deployment posture**
   (ROADMAP §8.2). Self-hosted operators legitimately call internal services; a
   hosted tier must never allow it. Deny-by-default plus per-credential host
   allowlists is my answer for both, but the flag's default is an operator
   decision that should be made explicitly rather than inherited.
