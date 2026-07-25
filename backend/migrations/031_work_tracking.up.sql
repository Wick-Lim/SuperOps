-- 031: work tracking — projects, issues, states, labels, cycles, ordering.
--
-- Plan 03, rebased onto the tree. Four things it says are now wrong and are NOT
-- done here; see docs/plans/README.md rulings 6 and 7, and the notes below.
--
-- THE AUTHORIZATION MODEL, because everything else follows from it:
--
--   `project` and `issue` are ACL-NATIVE. authz.RegisterTx writes their
--   acl_object row IN THE SAME TRANSACTION as the row it describes, and
--   internal/authz's derivedTypes stays exactly {workspace, channel, file}.
--   This migration adds ZERO arms to acl_object_expected and ZERO arms to
--   acl_key_expected — which is also why it does not have to rebase onto
--   migration 028's recreated view.
--
--   A project is registered under the workspace and carries ONE acl_grant with
--   the `workspace` subject; every issue is registered under its project and
--   inherits that grant through acl_key_expected arm 5's subtree join. That is
--   byte-for-byte the Drive-root shape.
--
--   A PROJECT IS NOT A CONTAINER in the acl_container_key sense, and must not
--   become one. Adding a `p-` prefix would mean widening acl_key_shape's closed
--   class AND internal/search's keyPrefixes — and a key that fails that
--   validator is DROPPED, which widens the filter it was meant to narrow
--   (README ruling 3). Migration 032 is HELD for that change if private
--   projects ever need it; nothing else may spend it.

-- ---------------------------------------------------------------------------
-- Projects
-- ---------------------------------------------------------------------------

CREATE TABLE projects (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),

    -- The prefix in an issue key: PROJ-14. Short, uppercase, and stable — it is
    -- printed on things, pasted into chat and typed into a search box, so
    -- renaming it would break every reference that already exists.
    key          TEXT NOT NULL CHECK (key ~ '^[A-Z][A-Z0-9]{1,9}$'),

    description  TEXT NOT NULL DEFAULT '',
    archived_at  TIMESTAMPTZ,

    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- One prefix per workspace. Two projects called PROJ makes PROJ-14 ambiguous,
-- which is the one thing an issue key must never be.
CREATE UNIQUE INDEX idx_projects_key ON projects (workspace_id, upper(key));
CREATE INDEX idx_projects_workspace ON projects (workspace_id) WHERE archived_at IS NULL;

CREATE TRIGGER trg_projects_updated_at
    BEFORE UPDATE ON projects FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Issue numbers
-- ---------------------------------------------------------------------------

-- A row counter, NOT a sequence, and not a column on projects.
--
-- A SEQUENCE is non-transactional: a rolled-back issue burns its number, and the
-- gap is indistinguishable from a deleted issue. In a tracker whose keys people
-- read aloud, "where did PROJ-13 go" is a support question that has no answer.
-- A row counter is transactional, so a rollback returns the number.
--
-- Its own table rather than projects.issue_counter because every issue creation
-- takes a row lock on it, and putting that lock on the projects row would
-- serialise renaming a project behind creating an issue in it.
--
-- The serialisation costs nothing new: authz.RegisterTx already takes
-- SELECT ... FOR UPDATE on the parent's acl_object row, so issue creation is
-- already serialised per project.
CREATE TABLE issue_counters (
    project_id UUID PRIMARY KEY REFERENCES projects(id) ON DELETE CASCADE,
    next_value INT NOT NULL DEFAULT 1 CHECK (next_value > 0)
);

-- ---------------------------------------------------------------------------
-- States
-- ---------------------------------------------------------------------------

-- Per-project rows over a fixed set of CATEGORIES.
--
-- The categories are what everything generic reasons about — "is it done", "is
-- it started" — and they are fixed because a workflow that could invent a
-- category would be a workflow no report could summarise. The rows are
-- per-project because "In review" means different things to two teams and a
-- shared list makes one of them wrong.
CREATE TABLE issue_states (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    category   TEXT NOT NULL CHECK (category IN
                   ('backlog', 'unstarted', 'started', 'completed', 'cancelled')),

    -- Column order on a board. An integer, not a rank: columns are few, reordered
    -- rarely, and by one person at a time.
    position   INT NOT NULL DEFAULT 0,
    color      TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- The target of the composite foreign key from issues. Without it, plan 03's
    -- plain `REFERENCES issue_states(id)` permits an issue in project A to hold
    -- a state belonging to project B — which does not error, does not appear in
    -- project A's column list, and therefore makes the issue vanish from its own
    -- board with no way to find it.
    UNIQUE (project_id, id)
);

CREATE INDEX idx_issue_states_project ON issue_states (project_id, position);

-- ---------------------------------------------------------------------------
-- Labels and cycles
-- ---------------------------------------------------------------------------

-- Workspace-scoped, not per-project. A label is a vocabulary ("bug", "customer")
-- and duplicating it per project means a cross-project view that cannot group by
-- it.
CREATE TABLE labels (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    name         TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 60),
    color        TEXT NOT NULL DEFAULT '',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE UNIQUE INDEX idx_labels_name ON labels (workspace_id, lower(name));

CREATE TABLE cycles (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    project_id UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    name       TEXT NOT NULL CHECK (length(name) BETWEEN 1 AND 120),
    starts_at  DATE NOT NULL,
    ends_at    DATE NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT cycles_ordered CHECK (ends_at >= starts_at)
);

CREATE INDEX idx_cycles_project ON cycles (project_id, starts_at DESC);

-- ---------------------------------------------------------------------------
-- Issues
-- ---------------------------------------------------------------------------

CREATE TABLE issues (
    id           UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    project_id   UUID NOT NULL REFERENCES projects(id) ON DELETE CASCADE,

    -- The number half of PROJ-14. Allocated from issue_counters.
    number       INT NOT NULL CHECK (number > 0),

    title        TEXT NOT NULL CHECK (length(title) BETWEEN 1 AND 500),
    description  TEXT NOT NULL DEFAULT '',

    state_id     UUID NOT NULL,

    assignee_id  UUID REFERENCES users(id) ON DELETE SET NULL,
    cycle_id     UUID REFERENCES cycles(id) ON DELETE SET NULL,

    -- 0 = none, then 1 urgent .. 4 low. Ordered so a sort is a sort.
    priority     SMALLINT NOT NULL DEFAULT 0 CHECK (priority BETWEEN 0 AND 4),

    -- THE ORDER, and there is exactly ONE.
    --
    -- Not one rank per view. A per-view rank means dragging a card in the board
    -- leaves the backlog untouched, so the two disagree and neither is "the"
    -- order — and every new view is a new column plus a backfill. One rank means
    -- a drag anywhere is a drag everywhere, which is what a user expects from a
    -- list of the same things.
    --
    -- TEXT with COLLATE "C" is the whole representation. C collation is
    -- byte-order, which is what makes "between a and b" a string operation with
    -- no locale in it: under any other collation 'a' < 'B' is a question about
    -- language, and a rank that sorted differently on two servers would reorder
    -- the board when a replica answered.
    rank         TEXT COLLATE "C" NOT NULL,

    completed_at TIMESTAMPTZ,
    archived_at  TIMESTAMPTZ,

    created_by   UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- PROJ-14 is unique.
    UNIQUE (project_id, number),

    -- THE COMPOSITE FOREIGN KEY. See issue_states: a plain reference to
    -- issue_states(id) lets an issue hold another project's state, and the issue
    -- then disappears from its own board.
    FOREIGN KEY (project_id, state_id) REFERENCES issue_states (project_id, id),

    -- The rank alphabet, enforced. A rank outside it sorts unpredictably against
    -- the ones inside it, and the failure is a board that reorders itself.
    CONSTRAINT issues_rank_shape CHECK (rank ~ '^[0-9A-Za-z]+$')
);

-- THE BOARD QUERY'S INDEX: one column of one project, in rank order.
CREATE INDEX idx_issues_board ON issues (project_id, state_id, rank, id)
    WHERE archived_at IS NULL;
-- The backlog / list view: one project, in rank order.
CREATE INDEX idx_issues_rank ON issues (project_id, rank, id) WHERE archived_at IS NULL;
-- "My issues", across projects.
CREATE INDEX idx_issues_assignee ON issues (assignee_id, updated_at DESC)
    WHERE assignee_id IS NOT NULL AND archived_at IS NULL;
CREATE INDEX idx_issues_cycle ON issues (cycle_id) WHERE cycle_id IS NOT NULL;
CREATE INDEX idx_issues_workspace ON issues (workspace_id, updated_at DESC);

CREATE TRIGGER trg_issues_updated_at
    BEFORE UPDATE ON issues FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ---------------------------------------------------------------------------
-- Labels on issues, and relations between them
-- ---------------------------------------------------------------------------

CREATE TABLE issue_labels (
    issue_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    label_id UUID NOT NULL REFERENCES labels(id) ON DELETE CASCADE,
    PRIMARY KEY (issue_id, label_id)
);

CREATE INDEX idx_issue_labels_label ON issue_labels (label_id);

CREATE TABLE issue_relations (
    source_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    target_id UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,

    -- 'blocks' is directional and 'relates' is not; the handler writes both
    -- directions for 'relates' so a reader never has to check two columns.
    kind      TEXT NOT NULL CHECK (kind IN ('blocks', 'relates', 'duplicates')),

    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    PRIMARY KEY (source_id, target_id, kind),
    CONSTRAINT issue_relations_not_self CHECK (source_id <> target_id)
);

CREATE INDEX idx_issue_relations_target ON issue_relations (target_id, kind);

-- ---------------------------------------------------------------------------
-- Activity
-- ---------------------------------------------------------------------------

-- What changed, when, by whom. Distinct from internal/audit: that is a
-- tamper-evident compliance record with a hash chain, and this is a feature —
-- the timeline rendered under an issue. Putting product history into the audit
-- log would make the compliance surface a mixture of both.
CREATE TABLE issue_activity (
    id         UUID PRIMARY KEY DEFAULT uuid_generate_v4(),
    issue_id   UUID NOT NULL REFERENCES issues(id) ON DELETE CASCADE,
    actor_id   UUID REFERENCES users(id) ON DELETE SET NULL,

    -- '<field>.<verb>': state.changed, assignee.set, label.added.
    kind       TEXT NOT NULL CHECK (kind ~ '^[a-z_]{1,24}\.[a-z_]{1,24}$'),
    -- Free-form, and deliberately not a foreign key to anything: it records what
    -- a field WAS, and the row it named may since have been deleted.
    from_value TEXT NOT NULL DEFAULT '',
    to_value   TEXT NOT NULL DEFAULT '',

    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_issue_activity_issue ON issue_activity (issue_id, created_at DESC);
