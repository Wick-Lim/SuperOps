# Plan 04 — Docs

**Phase 3.** The first editor on the Drive registry, and the phase that proves
the collaboration layer against a real workload.

Status: design. Depends on Phase 0 (plan 00 permissions, `internal/collab`),
Phase 1 (Drive + editor registry) and Phase 2 (comments). It depends on nothing
in Phase 4 or 5, and everything it establishes — the projection pipeline, the
embed contract, the Yjs provider — is what those two inherit.

---

## What it is

A Notion-style block editor on a Drive object. You create a document from
Drive or from a slash command, it opens in a full-pane editor, and several
people type in it at once with each other's cursors visible. Blocks are
paragraphs, headings, lists, checkboxes, quotes, code, dividers, tables, images,
and four SuperOps-specific embeds: a Drive file, another document, an issue, and
a user mention. `/` opens a command menu, `@` opens a mention menu. Comments
anchor to a text range and reuse Phase 2's comment surface. Search finds the
document by body text alongside messages and files. Permissions are whatever
Drive says they are — there is no per-document sharing UI in this phase, because
plan 00 already gives Drive one.

On mobile the document renders read-only and you can comment. That is ROADMAP
§3 option (1), taken as recommended: TipTap/ProseMirror is DOM-only, and the
editor pane exists only in the web bundle.

**Explicitly not in scope:** databases, table views, filters, rollups, formulas
in tables; public web publishing; per-block or per-document ACLs beyond what
Drive inherits; a second page hierarchy; offline editing; named version history
and diffs; import from Notion/Word; PDF/DOCX export; AI anything; mobile
editing. Each of those is a re-planning trigger, not a backlog item — the
reasons are in [Cuts](#cuts).

---

## The editor foundation

**TipTap v3 (ProseMirror) + `y-prosemirror` + Yjs, with our own block-UX layer.**

This is the plan's most consequential decision, so here is the reasoning rather
than the conclusion.

The CRDT is already chosen for us. `migrations/015_collab.up.sql` stores opaque
update blobs and states plainly that there is no server-side merge, and
`internal/ws/protocol.go:56-98` already speaks `collab.join` / `collab.update` /
`collab.awareness` in Yjs's shape. So the editor must have a mature **Yjs**
binding. That is the first filter and it is decisive.

| Option | Yjs binding | Block UX | Verdict |
|---|---|---|---|
| **TipTap 3 / ProseMirror** | `y-prosemirror`, written by the Yjs author; the reference binding | written by us | **chosen** |
| BlockNote | `y-prosemirror` under its own abstraction | free — slash menu, drag handles, nesting | rejected, see below |
| Lexical | `@lexical/yjs` exists; less exercised on complex custom nodes | written by us | rejected — same work as TipTap, weaker binding |
| Hand-rolled block model over `Y.Array<Y.Map>` | direct | written by us | rejected — selection, IME, undo across blocks is a year |

**Why not BlockNote**, which is the tempting "configured, not written" answer:
it would genuinely save the slash menu, drag handles and nested-block
containers, which is real (L-sized) work. Three things outweigh it.

1. Our four embed nodes are not decoration. A `driveEmbed` node must carry
   *only* `{ref_type, ref_id}` and resolve its preview per-caller at render time
   (see [The hard part](#the-hard-part)); the NodeView needs the full
   ProseMirror API, and BlockNote's custom-block API is a second abstraction to
   fight for exactly the nodes that matter most.
2. The document schema is a data-migration surface we cannot migrate (see
   [Risks](#risks)). Owning it outright is worth more than owning it partially.
3. Licensing: TipTap core + `@tiptap/pm` + `@tiptap/starter-kit` are MIT, as are
   ProseMirror, Yjs and `y-prosemirror`. BlockNote's core is MPL-2.0 with
   separately-licensed `xl-*` packages. For a self-hosted product shipped to
   customers that is a review we would rather not run.

Two guards on the choice:

- **No TipTap Pro packages.** `@tiptap/extension-drag-handle` and friends are
  behind a paid registry. We write the drag handle. If someone reaches for a
  Pro package, that is the moment to re-open BlockNote instead, because paying
  for a proprietary editor plugin inside a self-hosted product is the worst of
  both.
- **The Yjs provider is ours and is editor-independent.** `y-prosemirror` takes
  a `Y.Doc` and an `Awareness`, not a provider, so the SuperOps transport binding
  lives in `app/src/collab/` and is shared verbatim by the spreadsheet and the
  design surface. Writing it inside the docs editor would be the first of the
  three-implementations failure ROADMAP §4 exists to prevent.

**Block model.** A ProseMirror document, not a `Y.Array` of blocks. Top-level
nodes are the blocks; every block node carries a stable `blockId` attr (uuid,
assigned on create) so comments, backlinks and outline entries can address a
block without addressing a text offset. Nesting is `bulletList`/`orderedList`/
`taskList` plus one `toggle` node with a content child — deliberately *not*
Notion's arbitrary indent-anything model, which is where the nested-container
schema pain lives. Indent-any-block is a cut.

---

## Data model

**Migration 017** (013 is taken; 014 SSO and 015 collab are on disk; 016 is the
unified-search/inbox work in flight). If Drive and work tracking claim 017 and
018 first, this becomes the next free number — nothing here depends on their
ordering, only on the `files` row and Phase 2's `comments` table existing.

A document *is* a Drive file (ROADMAP §3b). The `documents` table holds only
what Drive does not: everything about naming, foldering, trash, sharing, ACLs,
versions and activity stays in Drive, and this table is deliberately thin as the
proof that the registry is real rather than decorative.

```sql
-- The editor's half of a Drive object. files.name is the title; files' folder
-- is the location; plan 00's acl_object row is the permission. None of that is
-- duplicated here, and a duplicate would immediately be a second source of
-- truth for permissions.
CREATE TABLE documents (
    file_id      UUID PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,

    -- The collaboration room. Created lazily on first open, so a document
    -- imported or created in bulk costs nothing until someone opens it.
    -- NOTE: the collab protocol addresses rooms by collab_documents.id, not by
    -- file id (internal/ws/room.go:134 takes document_id straight to the room
    -- handler). Conflating the two is the obvious first bug of this phase.
    collab_document_id UUID UNIQUE REFERENCES collab_documents(id) ON DELETE SET NULL,

    icon          TEXT NOT NULL DEFAULT '' CHECK (char_length(icon) <= 16),
    cover_file_id UUID REFERENCES files(id) ON DELETE SET NULL,

    -- The block schema this document was last written with. A client whose
    -- schema version is lower refuses to open it rather than silently stripping
    -- nodes it does not know — see Risks.
    schema_version INT NOT NULL DEFAULT 1,

    -- The projection: a DERIVED, client-published, non-authoritative rendering
    -- of the CRDT. Never a source of truth for content; the source of truth is
    -- collab_updates. See "The hard part".
    projection_seq BIGINT NOT NULL DEFAULT 0,
    projection_at  TIMESTAMPTZ,
    body_text      TEXT  NOT NULL DEFAULT '',
    outline        JSONB NOT NULL DEFAULT '[]',   -- [{block_id, level, text}]

    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- 1 MiB of extracted text is ~250 printed pages. Beyond that the document
    -- is being used as a database and the projection is not the problem.
    CONSTRAINT documents_body_text_bounded CHECK (octet_length(body_text) <= 1048576)
);

CREATE INDEX idx_documents_workspace_updated ON documents (workspace_id, updated_at DESC, file_id DESC);
-- The reconciler's query: documents whose projection has fallen behind.
CREATE INDEX idx_documents_projection_stale ON documents (workspace_id, projection_at)
    WHERE projection_at IS NULL OR projection_seq = 0;
```

```sql
-- Every typed object the document points at. Rebuilt wholesale inside the
-- projection transaction, so it is exactly as fresh as the projection and never
-- more. Backs backlinks, "where is this file used", and mention notifications.
CREATE TABLE document_refs (
    document_id UUID NOT NULL REFERENCES documents(file_id) ON DELETE CASCADE,
    ref_type    TEXT NOT NULL CHECK (ref_type ~ '^[a-z][a-z0-9_]{0,31}$'),
    ref_id      UUID NOT NULL,
    block_id    TEXT NOT NULL DEFAULT '' CHECK (char_length(block_id) <= 64),
    PRIMARY KEY (document_id, ref_type, ref_id, block_id)
);
-- The reverse lookup: "which documents embed this file / mention this user".
CREATE INDEX idx_document_refs_target ON document_refs (ref_type, ref_id, document_id);
```

```sql
-- The anchor half of a comment. The comment itself — body, thread, author,
-- resolution, notifications — is Phase 2's `comments` row; only the position is
-- new, because a position in a CRDT is not an integer offset.
CREATE TABLE document_comment_anchors (
    comment_id  UUID PRIMARY KEY REFERENCES comments(id) ON DELETE CASCADE,
    document_id UUID NOT NULL REFERENCES documents(file_id) ON DELETE CASCADE,
    -- Yjs encoded relative positions. Opaque: the server never decodes one, for
    -- the same reason it never merges an update.
    anchor_start BYTEA NOT NULL CHECK (octet_length(anchor_start) BETWEEN 1 AND 4096),
    anchor_end   BYTEA NOT NULL CHECK (octet_length(anchor_end)   BETWEEN 1 AND 4096),
    -- The quoted text at creation time. Shown in the sidebar, and the only
    -- thing left when the anchored range is deleted — an orphaned comment must
    -- still be readable, or resolving a discussion destroys its subject.
    quote    TEXT NOT NULL DEFAULT '' CHECK (char_length(quote) <= 2000),
    block_id TEXT NOT NULL DEFAULT ''
);
CREATE INDEX idx_document_comment_anchors_doc ON document_comment_anchors (document_id);
```

One enum change: `notification_type` (migration 005) is a Postgres enum, so
`ALTER TYPE notification_type ADD VALUE 'doc_mention'` and `'doc_comment'` go in
017. `ADD VALUE` cannot run inside a transaction block in older Postgres — check
how `cmd/migrate` wraps statements before writing it, or it fails on first
deploy.

### What is deliberately absent

- **No `parent_document_id`.** Sub-pages are Drive files in a Drive folder, and
  the "sub-page" block is a `docEmbed` ref. Two hierarchies means two
  inheritance stories, and plan 00's `acl_object.path` is already the one.
- **No `blocks` table.** The block tree lives in the Y.Doc. A relational mirror
  would be a second writer to the same content and would need server-side merge.
- **No `document_versions`.** A version is a `collab_snapshots` row; naming and
  restoring versions is Drive's feature applied to that table, in Phase 1.

---

## API surface

`internal/document.Handler` with the house shape —
`RegisterRoutes(mux, authMw)` like `internal/file/handler.go:47`,
`{data,meta,error}` via `pkg/httputil`, keyset cursors via
`httputil.ParsePagination` / `EncodeCursor`.

```
POST   /api/v1/workspaces/{workspace_id}/documents
GET    /api/v1/workspaces/{workspace_id}/documents          list, keyset (updated_at, file_id)
GET    /api/v1/documents/{document_id}
PATCH  /api/v1/documents/{document_id}                      title, icon, cover
DELETE /api/v1/documents/{document_id}                      → Drive trash, not a hard delete

POST   /api/v1/documents/{document_id}/projection           the derived rendering
GET    /api/v1/documents/{document_id}/content              read-only blocks JSON (mobile, previews)
GET    /api/v1/documents/{document_id}/export?format=md

POST   /api/v1/documents/{document_id}/embeds/resolve       per-caller preview resolution
GET    /api/v1/documents/{document_id}/backlinks            keyset
GET    /api/v1/objects/{ref_type}/{ref_id}/documents        "used in", keyset

POST   /api/v1/documents/{document_id}/comments             body + anchor
GET    /api/v1/documents/{document_id}/comments             keyset
```

`GET /api/v1/documents/{id}` returns `head_seq` and `projection_seq` alongside
the metadata. That is not debug output: a client uses the gap to decide whether
the read-only render it is about to show is stale, and an operator uses it to
see the projection pipeline failing before a user reports unsearchable
documents.

**Routes this plan does not add**, because `internal/collab` owns them: opening
a room (`collab.join` over the WS hub), fetching state, appending an
oversized update over HTTP, and posting a snapshot. If those endpoints do not
exist when this phase starts, they belong in the collab plan, not here.

Authorization, first statement of every handler body, per `internal/authz`'s
stated rules: resolve the document's `file_id`, ask plan 00's checker for the
effective capability on `("file", file_id)`, and map `read` → GET, `comment` →
POST comments, `write` → PATCH/projection, `share`/`admin` → nothing in this
phase. `err != nil` → 500, insufficient capability → 403/404, never collapsed.
The list endpoint filters on `acl_key` and never calls `Can` per row — plan 00
names that N+1 as the failure mode the whole design exists to prevent.

---

## Package layout

**New:**

- `internal/document` — handler, repository, projection validation, ref
  extraction, embed resolution, markdown export. Owns the three tables above and
  nothing else.
- `app/src/collab/` — the Yjs provider over the existing WS manager. **Not
  docs-specific**; the spreadsheet and design surface import it unchanged.
  `provider.ts` (join/leave, update fan-in and fan-out, awareness, watermark and
  gap handling, HTTP fallback for oversized updates), `awareness.ts`.
- `app/src/editor/` — `schema.ts`, `extensions/` (slash menu, drag handle,
  mention, four embed NodeViews, block ids), `Editor.web.tsx`,
  `Editor.native.tsx`, `projection.ts` (block tree → text + outline + refs).
- `app/src/api/documents.ts`, `app/src/screens/DocumentScreen.tsx`.

**Reused, not rebuilt:**

| Need | Comes from |
|---|---|
| CRDT transport, persistence, awareness, revocation | `internal/collab` + `internal/ws/room.go` |
| Permissions, inheritance, `acl_key` filtering | plan 00's checker via `internal/authz` |
| Storage, folders, trash, versions, sharing, activity | Drive (Phase 1), `internal/file` |
| Comment bodies, threads, resolution, notifications | Phase 2's comment surface |
| Search index, ACL keys, worker indexer | `internal/search` — `TypeDocument` is already declared at `internal/search/doc.go:25` |
| Mention notifications + dedupe | `internal/notification`, `notificationID` at `service.go:42` |
| Durable jobs, retries, singleton locks | `cmd/worker` — `bindDurable:527`, `runLoop:627`, `withSingletonLock:673` |
| Inline markdown rendering on mobile | `app/src/components/message/RichText.tsx` |

**Dependencies.** Zero new Go modules. Client: `yjs`, `y-protocols`,
`y-prosemirror`, `@tiptap/core`, `@tiptap/pm`, `@tiptap/react`,
`@tiptap/starter-kit`, `lib0` (transitive) — all MIT. All of them are imported
only from `.web.tsx` files, so Metro's platform resolution keeps roughly 400 KB
of DOM editor out of the iOS and Android bundles. That is a verifiable claim and
[Verification](#verification) verifies it.

---

## The hard part

**The server cannot read the document, and six product features need to.**

This is the structural consequence of "CRDT in the client, opaque to the
server" (ROADMAP §4, made concrete in migration 015) colliding with what a
document is actually for. Search must index the body. Mobile must render it.
A link in a channel must preview it. Export must produce markdown. A mention
must notify. Backlinks must exist. Not one of those can be served from a `BYTEA`
column of Yjs updates, and the three answers that look available are all wrong:

- *Implement Yjs in Go.* There is no mature implementation, and a Go merge that
  disagreed with the client's would be a corruption bug debuggable from neither
  side. Migration 015 rejects this in its header comment; so do we.
- *Run a headless Node sidecar with the editor schema.* Correct, and it adds a
  second runtime to every deployment, a version-lockstep between it and the web
  bundle, and a §3c capability decision to a phase that should not need one.
- *Give up and search titles only.* This is a documents product.

### The answer: a client-published projection, treated as derived state

The client that already has the document in memory renders it to
`{seq, text, outline, refs[], schema_version}` and POSTs it. The server stores
it, indexes it, and never treats it as content. Five rules make that safe:

1. **Monotonic on seq, in one statement.** `UPDATE documents SET ... WHERE
   file_id = $1 AND projection_seq < $2` and `$2 <= head_seq` read from
   `collab_documents` in the same transaction. Two clients projecting the same
   document race harmlessly; the loser's update matches zero rows. A projection
   claiming a seq above the log head is rejected — it is either a bug or a
   client inventing content.
2. **One projector per room.** The client that projects is the room leader,
   chosen by `Hub.SendToRoomLeader` (`internal/ws/room.go:377`) — the same
   mechanism compaction already uses, for the same reason. Rule 1 makes it safe
   for several to try anyway, which is what happens when a room spans replicas.
3. **Authorized on write capability at POST time, not at join time.** The room
   membership check is cached in memory for the keystroke path
   (`internal/ws/room.go` documents that trade). The projection endpoint is
   HTTP and re-checks, so a user revoked mid-session cannot land one last
   rewrite of the searchable body.
4. **Bounded.** ≤1 MiB text, ≤1000 refs, ≤500 outline entries, ≤64-byte block
   ids. A projection is attacker-supplied by definition; every one of those is a
   400, not a truncation.
5. **Never authoritative.** Losing the whole `documents.body_text` column costs
   search and mobile rendering until re-projection, and costs zero content. The
   drop test for any proposed use of the projection is: *would corrupting it
   lose a user's writing?* If yes, the design is wrong.

**When the client projects:** 2 s after the last local change settles, on blur,
on room leave, and on demand. On-demand needs one new outbound WS frame,
`collab.project`, next to `TypeCollabCompact` (`internal/ws/protocol.go:98`) —
the identical mechanism, asking the leader for a projection instead of a
snapshot.

**When nobody is in the room** — the case that actually bites in production. A
document edited and closed before the debounce fires is unsearchable forever
unless something notices. A fifth worker job loop, `documentProjection`, built
on `runLoop`/`withSingletonLock`, scans for `head_seq - projection_seq > 200 OR
(projection_at < now() - interval '15 minutes' AND head_seq > projection_seq)`,
asks the room leader if there is one, and otherwise records the document as
stale and moves on. It **never blocks and never guesses at content**: a stale
document keeps its last good projection in the index and is counted in a metric.
The next person to open it projects it. Fail visible, never fail silent.

### The security face of the same problem: embeds

A document embeds a Drive file. The document is shared with someone who cannot
read that file. The body is an opaque blob the server cannot filter, so the
only defence is that **the body never contains anything worth leaking**.

The rule, enforced in the schema and in review: an embed node's attrs are
`{ref_type, ref_id}` and nothing else. No title, no filename, no thumbnail URL,
no issue summary. The NodeView renders a placeholder and calls
`POST /documents/{id}/embeds/resolve` with the ref list on screen; the server
resolves each one against **the caller's** capability and returns either a
preview or `{"access": "denied"}`. Consequences worth stating:

- The document renders with grey placeholders for the caller who cannot read the
  target. That is the correct UX and it is also the honest one.
- Copying a document, sharing it into a channel, or exporting it cannot leak a
  title, because no copy of the title exists outside the target object.
- `document_refs` is a *stored* claim by a client and is therefore never used to
  authorize anything — only to find candidates, each of which is checked.
- A user who types the file's name as literal text has leaked it. That is not a
  problem we can solve and not one we should pretend to.

### Comment anchors

The third face. A comment on "this paragraph" cannot store an offset, because
concurrent edits move it. Anchors are Yjs **encoded relative positions**
(`Y.encodeRelativePosition`), produced and resolved entirely in the client and
stored as opaque `BYTEA`. Anchors that no longer resolve become orphaned
comments in the sidebar with their `quote` intact rather than disappearing —
which is why `quote` is denormalized despite being a copy.

---

## Sequencing inside the phase

1. **Document object + registry entry** — S. Create, read, rename, trash a
   document with an empty body; a Drive file with content type
   `application/vnd.superops.document`, opening its collab room lazily. Ships
   first because it proves both seams (Drive registry, collab room) with no
   editor at all, and because it exposes the Drive gap early:
   **a document file has no MinIO object**, so `files.storage_key` must tolerate
   a handler-produced body and `runObjectGC` (`cmd/worker/main.go:1121`) must
   not treat it as anything. If Drive cannot express a bodyless file, that is a
   Phase 1 change and we want to know in week one.
2. **Yjs provider + editor shell** — L, **the long pole**. Provider over the
   existing WS manager, `y-prosemirror` binding, presence cursors from awareness,
   reconnect and watermark handling, HTTP fallback for updates over
   `maxCollabPayloadBytes` (`internal/ws/client.go:39`, 32 KiB — a pasted table
   exceeds it easily and the WS path will reject it).
3. **Projection pipeline + search** — M. Endpoint, worker consumer, `DocumentDoc`
   in `internal/search`, reconciler job loop.
4. **Block UX** — L, and it shares an engineer with (2). Slash menu, drag handle,
   nesting, keyboard map, IME, markdown input rules, undo scoping via Yjs
   `UndoManager` (naive undo in a collaborative editor undoes *other people's*
   edits — this is a known trap and the binding solves it only if it is wired).
5. **Embeds + backlinks** — M. Parallel with (3) once refs exist. Needs Phase 2
   for the issue embed; the file and document embeds do not.
6. **Comments + anchors** — M. Parallel with (5), gated on Phase 2 landing a
   comment surface keyed on `(object_type, object_id)`.
7. **Mobile read-only + markdown export** — S–M. Parallel, and a different
   engineer. Both consume the projection, so both are cheap once (3) is done.

Parallelizable: (3)+(5)+(7) backend, against (2)+(4) client. The long pole is
(2)+(4) on one client engineer, and it does not parallelize with itself.

---

## Risks and failure modes

**Schema change is a data migration you cannot run.** The worst one, and it only
appears after shipping. ProseMirror strips nodes its schema does not know when
it loads a document — and `y-prosemirror` will sync that deletion back to
everyone. So one stale browser tab joining a room after a node type is added
silently deletes every instance of it, for everyone, permanently. Mitigations,
all mandatory: node types are **added only, never removed or renamed**; every
custom node parses down to a paragraph rather than being dropped;
`documents.schema_version` is bumped on write and a client whose compiled schema
version is lower **refuses to open the document** and asks the user to reload,
rather than opening it read-only (read-only is not enough — a mounted
`y-prosemirror` binding can write on load). This is also why the schema is ours
rather than a library's.

**Yjs document growth.** A long-lived document accumulates tens of thousands of
`collab_updates` rows, and load time is snapshot + tail. The collab layer
compacts; Docs is the first workload that will actually exercise it. Verify at
50k updates before anyone writes a real document in it, not after.

**Projection storms.** Five people idle simultaneously in a busy document → five
projections, five index writes. Leader election plus the conditional update makes
four of them no-ops at the database, but the requests still arrive. Rate-limit
the projection endpoint per document, and index only when `body_text`'s hash
changed.

**Search index churn.** Every projection reindexes the whole document. A
workspace with a hundred active documents produces steady Meilisearch write
pressure, and `ensureSettings` (`internal/search/service.go:165`) exists because
settings churn triggers full re-indexes. Coalesce per document in the worker
consumer.

**Snapshot ceiling.** `collab_snapshots.payload` caps at 16 MiB (migration 015).
A document with base64 images inline blows through it and then cannot be
compacted — which fails silently as unbounded log growth. Rule: images are Drive
files, always. Enforced by having no image-data node in the schema.

**Revocation during an edit.** Handled for delivery by `Hub.RevokeRoom`
(`internal/ws/room.go:424`), which plan 00's `Revoke`/`Move` must call for
`("file", id)` objects. The gap it does not close is the projection endpoint,
which is why rule 3 above re-checks.

**Unknown blocks on mobile.** A block type shipped to web before the native
renderer knows it must render as its plain text, never as a blank. Otherwise the
first schema addition makes documents look empty on phones.

**Two writers to the title.** Title is `files.name`, a plain field, not part of
the CRDT. Concurrent renames are last-write-wins. That is a deliberate cut, and
it is the one place in the document where a lost update is possible.

**The `acl_key` gap** (see [Open gaps](#open-gaps-against-plan-00)) is a
correctness risk, not a performance one: get it wrong and document search either
returns nothing or returns too much.

---

## Verification

New file `backend/test/integration/documents_test.go`, in the existing harness
(real Postgres/Redis/NATS; the harness already builds the whole app via
`app.New`).

- `TestDocumentCreateOpensCollabRoom` — create → `collab_documents` row with
  `resource_type='document'`, `resource_id=files.id`; second open reuses it.
- `TestCrossTenantDocumentAccess` — modelled on `TestCrossTenantChannelAccess`
  (`tenancy_test.go:88`). Every document route, from another workspace, 403/404.
- `TestProjectionRejectsStaleSeq` — two projections out of order; the older one
  matches zero rows and the newer body survives.
- `TestProjectionRejectsFutureSeq` — `seq > head_seq` is a 400.
- `TestProjectionRequiresWriteCapability` — a `comment`-capability caller gets
  403; a `read` caller gets 403.
- `TestRevokedEditorCannotProject` — join, revoke, project → 403. The room
  membership cache must not carry the HTTP path.
- `TestEmbedResolveDeniesUnreadableTarget` — the leak test, and the one to run
  in CI on every commit: a document embedding a file the caller cannot read
  resolves to `{"access":"denied"}` with **no title field present in the JSON**.
- `TestDocumentSearchRespectsACL` — needs Meilisearch. **CI has no Meilisearch
  and no MinIO service** (`.github/workflows/ci.yml:76-91` runs only Postgres
  and Redis, with NATS started by hand), so today this test would skip in the
  one place it must not. Adding a `getmeili/meilisearch` service to CI is part
  of this phase, not a nice-to-have — search is half the value of Docs and the
  harness comment already records that skipping-everywhere is how the suite went
  green for months on a broken config.
- `TestBacklinksAppearAndDisappear` — refs are rebuilt wholesale, so removing an
  embed removes the backlink.
- `TestDocMentionNotifiesOnce` — re-projecting the same document does not
  re-notify, via the existing derived-id dedupe (`notification/service.go:42`).

WebSocket-level, in `realtime_test.go`'s style: two clients join the same room,
one sends recorded Yjs update fixtures, both receive them in seq order with no
gap, and replaying the stored log yields the same byte sequence. Go cannot
compute Yjs convergence, so *convergence itself* is verified in the client suite
(`app/vitest.config.ts` already runs vitest): two `Y.Doc`s, the provider, a
mock socket, concurrent edits, assert identical `Y.Doc` state and identical
projection output.

Client unit tests: provider gap detection and resync; oversized-update HTTP
fallback; projection extractor (block tree → text, outline, refs) including the
"no title in an embed node" invariant as a test, not a comment; native renderer
falling back on an unknown block type.

Two checks that are not unit tests and matter more than several that are:

- **Bundle check** — `expo export --platform ios` then grep the output for
  `prosemirror`. A hit means the platform split leaked and the mobile app grew
  400 KB for a feature it does not have.
- **Scale check** — a script that appends 50k updates to one document and
  measures load time, snapshot size and compaction behaviour. Run it before the
  first real document exists.

---

## Sizing

| Piece | Size |
|---|---|
| Document object, Drive registry entry, CRUD | S |
| Yjs provider + `y-prosemirror` binding + presence | **L — long pole** |
| Block UX (slash, drag, nesting, keyboard, IME, undo) | **L — same engineer** |
| Projection endpoint + worker consumer + reconciler | M |
| Search integration (`DocumentDoc`, reindex source) | S |
| Embeds + per-caller resolution + backlinks | M |
| Comments + relative-position anchors | M |
| Mobile read-only renderer + markdown export | S |

**L overall, concentrated in the client.** The backend is genuinely M — which is
the point of §3b, and is the number to check the registry against: if the
backend for the second editor is not also M, the registry did not work.

---

## Cuts

Each of these is a re-planning trigger.

| Cut | Why |
|---|---|
| Databases, table views, filters, rollups | This is Notion's other product. It needs a query engine, a schema editor and per-view permissions, and it is larger than the editor. |
| A second page hierarchy (`parent_document_id`) | Drive folders + plan 00's `acl_object.path` are the hierarchy. Two trees is two inheritance stories and an inevitable disagreement about which one an ACL follows. |
| Per-block and per-document ACLs | Plan 00 has no sub-object subject and inventing one here is exactly the "invent a different permission story" failure it was written to stop. |
| Public web publishing | An unauthenticated read path into a permission model built entirely around authenticated subjects. Deserves its own design, including cache, abuse and revocation. |
| Offline editing | Yjs makes it *look* free. It costs conflict UX, an unbounded local log, and a revocation hole — a client removed from a workspace can edit for a week and flush on reconnect. v1: read-only when disconnected; updates buffered for the reconnect window only. |
| Named versions, diff view, restore UI | Snapshots exist; naming and restoring them is a Drive-versioning feature that should be built once for all three editors. |
| PDF/DOCX export, Notion/Word import | Import is an open-ended fidelity treadmill; PDF export needs a renderer. Markdown covers the real need (move content out) at a fraction of the cost. |
| Collaborative title editing | One field, last-write-wins, and keeping it out of the CRDT is what lets the server read a title without the projection pipeline. |
| Indent-anything nesting | Notion's arbitrary block nesting is where ProseMirror schema pain concentrates. Lists and one toggle node cover the real use. |
| Mobile editing | ROADMAP §3, unchanged. Revisit when users ask, not before. |
| Reactions on blocks, per-block comments-as-threads | Comment ranges cover it; both add a second anchoring model. |

---

## Open gaps against plan 00

Stated as gaps, not worked around.

1. **No folder access key.** `internal/search/doc.go:109-125` closes the key
   prefix set at `w-`, `c-`, `u-`, `g-`, and `AccessKeys`
   (`internal/search/handler.go:42`) emits only workspace, user and channel
   keys. A document inheriting from a Drive folder has no key that expresses it.
   Docs is the first object type that is not channel-scoped, so this is where it
   bites: either an `f-<folder>` prefix is added and a caller's key set includes
   every folder they can read, or document search is wrong. Plan 00's `acl_key`
   table is the right home for the object side; the caller side is a one-line
   addition to `AccessKeys` and a reindex. **This needs to be settled in Phase 0
   or Phase 1, not discovered here.**
2. **Comments must be polymorphic.** Phase 2 must key its comment rows on
   `(object_type, object_id)`, not on `issue_id`. If it does not, this phase
   either forks the comment surface — which is the failure ROADMAP §1 names — or
   pays for a migration.
3. **The `comment` capability must be real.** Plan 00 defines
   `admin > share > write > comment > read`. Docs is the first consumer that
   distinguishes `comment` from `write` in a UI. Until plan 00 lands, comments
   are gated on `write`, which is wrong and should be a known temporary state
   rather than a shipped design.
4. **A Drive file with no stored object.** Phase 1 must allow a `files` row
   whose bytes are produced on demand by its registered handler, and the object
   GC must know the difference between that and an orphan. This is the registry
   contract (§3b) meeting reality, and it is not something Docs can decide
   alone.
5. **`internal/collab`'s public Go surface.** This plan assumes a service
   implementing `ws.RoomHandler` (`internal/ws/room.go:64`) plus a way to
   create/find a room for `(resource_type, resource_id)` and read `head_seq`.
   If that surface differs, only `internal/document`'s room-creation path
   changes — but it should be confirmed before day one rather than discovered on
   day three.
