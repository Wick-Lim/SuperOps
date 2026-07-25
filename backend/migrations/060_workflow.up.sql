-- 060: automation — when something happens, do something.
--
-- THE PROBLEM THIS SCHEMA EXISTS TO SOLVE IS AT-LEAST-ONCE DELIVERY. Every
-- trigger arrives on a durable JetStream consumer, which redelivers on any nak
-- and after any crash. A workflow that posts a message must therefore be able
-- to answer "did I already do this?" for every step, or a redelivered event
-- posts the message twice and the automation becomes the thing people turn off.
--
-- workflow_effects is that answer, and it is the load-bearing table here.

CREATE TABLE workflows (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    name        TEXT NOT NULL,
    description TEXT NOT NULL DEFAULT '',

    -- Disabled by default. A workflow that started running the moment it was
    -- saved would fire on the half-built version somebody was still editing.
    enabled BOOLEAN NOT NULL DEFAULT FALSE,

    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    archived_at TIMESTAMPTZ,

    CONSTRAINT workflows_name_bounded CHECK (char_length(name) BETWEEN 1 AND 200)
);

CREATE INDEX idx_workflows_workspace ON workflows (workspace_id) WHERE archived_at IS NULL;

-- Versions are IMMUTABLE. A run records which version it executed, so reading
-- a two-week-old run tells you what it actually did rather than what the
-- workflow does now — the difference between an audit trail and a guess.
CREATE TABLE workflow_versions (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version     INT  NOT NULL,

    -- The step list, in order. JSONB because the step vocabulary is a product
    -- decision that changes without a migration, and because nothing SQL-side
    -- ever queries inside it.
    steps JSONB NOT NULL DEFAULT '[]'::jsonb,

    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_versions_number_key UNIQUE (workflow_id, version),
    CONSTRAINT workflow_versions_steps_array CHECK (jsonb_typeof(steps) = 'array'),
    -- A workflow with a hundred steps is a program, and this is not a
    -- programming language. The bound is also what stops one trigger from
    -- turning into an unbounded amount of work.
    CONSTRAINT workflow_versions_steps_bounded CHECK (jsonb_array_length(steps) BETWEEN 0 AND 20),
    CONSTRAINT workflow_versions_steps_size CHECK (pg_column_size(steps) <= 65536)
);

-- What starts a workflow.
--
-- No SCHEDULE trigger in v1, and that is a cut rather than an omission: a
-- schedule needs a scheduler with its own catch-up semantics, its own
-- missed-window policy and its own timezone story, and none of that is shared
-- with event triggers.
CREATE TABLE workflow_triggers (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,

    -- The domain event, e.g. 'message.new' or 'issue.created'. A shape CHECK,
    -- not an enumeration: a new event type must not be a migration.
    event_type TEXT NOT NULL,
    -- Optional narrowing, e.g. {"channel_id": "..."}. Matched by the Go side.
    filter JSONB NOT NULL DEFAULT '{}'::jsonb,

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_triggers_event_shape CHECK (event_type ~ '^[a-z][a-z0-9_.]{0,63}$'),
    CONSTRAINT workflow_triggers_filter_object CHECK (jsonb_typeof(filter) = 'object'),
    CONSTRAINT workflow_triggers_one_per_event UNIQUE (workflow_id, event_type)
);

CREATE INDEX idx_workflow_triggers_event ON workflow_triggers (event_type);

CREATE TABLE workflow_runs (
    id          UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    version_id  UUID NOT NULL REFERENCES workflow_versions(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- THE IDEMPOTENCY KEY FOR THE WHOLE RUN. Derived from the triggering
    -- event's id, so a redelivered event finds the run it already started
    -- instead of starting a second one.
    trigger_key TEXT NOT NULL,
    event_type  TEXT NOT NULL,
    -- The event payload, frozen. A step reads from here rather than from the
    -- live database, so a run's behaviour does not change under it while it is
    -- executing.
    payload JSONB NOT NULL DEFAULT '{}'::jsonb,

    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'running', 'succeeded', 'failed', 'cancelled')),
    error  TEXT NOT NULL DEFAULT '',

    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT workflow_runs_trigger_key UNIQUE (workflow_id, trigger_key),
    CONSTRAINT workflow_runs_payload_object CHECK (jsonb_typeof(payload) = 'object'),
    CONSTRAINT workflow_runs_payload_bounded CHECK (pg_column_size(payload) <= 262144)
);

-- The claim query: pending runs, oldest first. FOR UPDATE SKIP LOCKED against
-- this index is how several workers share the queue without an advisory lock —
-- the same shape the scheduled-message promoter already uses.
CREATE INDEX idx_workflow_runs_pending ON workflow_runs (created_at)
    WHERE status IN ('pending', 'running');
CREATE INDEX idx_workflow_runs_workflow ON workflow_runs (workflow_id, created_at DESC);

CREATE TABLE workflow_step_runs (
    run_id     UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_index INT  NOT NULL,

    kind   TEXT NOT NULL,
    status TEXT NOT NULL DEFAULT 'pending'
           CHECK (status IN ('pending', 'succeeded', 'failed', 'skipped')),
    error  TEXT NOT NULL DEFAULT '',
    -- What the step produced, for the run detail view. Bounded, because a step
    -- that returned a whole document would make the run log the largest table
    -- in the database.
    output JSONB NOT NULL DEFAULT '{}'::jsonb,

    started_at  TIMESTAMPTZ,
    finished_at TIMESTAMPTZ,

    PRIMARY KEY (run_id, step_index),
    CONSTRAINT workflow_step_runs_output_bounded CHECK (pg_column_size(output) <= 16384)
);

-- THE TABLE THAT MAKES RETRIES SAFE.
--
-- Before a step performs a side effect it INSERTs its key here, in the same
-- transaction. A redelivery conflicts and the step is skipped, so the message
-- is posted once even though the event arrived three times.
--
-- The key is (run, step), which is stable across redeliveries precisely because
-- the run id is derived from the trigger key rather than minted per delivery.
CREATE TABLE workflow_effects (
    run_id     UUID NOT NULL REFERENCES workflow_runs(id) ON DELETE CASCADE,
    step_index INT  NOT NULL,
    kind       TEXT NOT NULL,
    -- What the effect produced, e.g. the message id, so a retry can report the
    -- original result rather than an empty success.
    result JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (run_id, step_index)
);

-- Triggers that matched a workflow and were REFUSED, with the reason.
--
-- Not a log for its own sake: the two ways automation goes wrong invisibly are
-- "it never fired" and "it fired and was silently dropped", and without this
-- both look identical to a user staring at a workflow that appears enabled.
CREATE TABLE workflow_trigger_rejections (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workflow_id  UUID NOT NULL REFERENCES workflows(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    event_type   TEXT NOT NULL,
    reason       TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_workflow_trigger_rejections_workflow
    ON workflow_trigger_rejections (workflow_id, created_at DESC);

CREATE TRIGGER trg_workflows_updated_at
    BEFORE UPDATE ON workflows FOR EACH ROW EXECUTE FUNCTION set_updated_at();
