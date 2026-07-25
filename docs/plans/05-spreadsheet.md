# Plan 05 — Spreadsheet

**Phase 4. The second editor.** Depends on Phase 0 (object permissions, the
collaboration layer, unified search) and Phase 1 (Drive + the editor registry).
Written against `internal/collab` + `migrations/015_collab.up.sql` as they land
from the in-flight batch, and against the code that exists today.

Status: design. Not started.

---

## 1. What it is

A spreadsheet opens from Drive like any other file. It is a workbook of tabs; a
tab is a grid of cells; a cell holds a literal (number, text, boolean, date) or
a formula that begins with `=`. Formulas reference other cells and ranges,
across tabs, and recalculate as you type. Several people edit the same workbook
at once with visible cursors and selections. Numbers carry a display format
(decimals, thousands, currency, percent, date). Rows and columns insert, delete,
resize, freeze. Copy/paste interoperates with Excel and Google Sheets via the
TSV clipboard flavour. CSV imports and exports.

That is the product. It is deliberately the smallest thing that a person doing
real work with a spreadsheet would accept.

**Not in scope, from the roadmap's own cut list (§7) plus what this plan adds:**

| Cut | Why |
|---|---|
| Pivot tables, charts, conditional formatting, macros, external data | ROADMAP §7. Each is a product. |
| Array formulas, dynamic arrays, spill ranges | A different evaluation model (a cell's result has a shape). Touches every function signature. |
| `INDIRECT`, `OFFSET` | §5.4 — they make the dependency graph data-dependent, which costs more than the two functions are worth. |
| `RAND`, `RANDBETWEEN` | §5.6 — nondeterministic across clients, and the whole convergence argument rests on determinism. |
| Protected ranges / per-cell permissions | Plan 00 grants a capability on an *object*. Sub-object capability is a gap; see §9. |
| Server-side formula evaluation (a Go engine) | §5.2. One engine. This is the single most important cut in the phase. |
| XLSX import/export | Would take `github.com/xuri/excelize/v2`. CSV covers v1; revisit as its own S-sized item. |
| Native mobile editing | ROADMAP §3, recommendation (1): web-first. Mobile opens read-only, under a cell cap (§8). |
| Undo across users | Yjs `UndoManager` scoped to the local client's origin only. "Undo someone else's edit" is out. |

---

## 2. What this phase does *not* build, because something else already did

This is the §3b payoff, and it should be checked before writing any code — if
this list is wrong, the plan is wrong.

| Need | Comes from | Evidence |
|---|---|---|
| The object, its ACL, sharing, versions, trash | Drive (Phase 1) | ROADMAP §3b |
| Multiplayer transport, persistence, awareness, reconnect | `internal/collab` + the hub's room layer | `internal/ws/room.go:63` (`RoomHandler`), `migrations/015_collab.up.sql` |
| "May this user open/edit this sheet" | `authz.Checker.Capability` (Plan 00), surfaced as `RoomAccess.CanWrite` | `internal/ws/room.go:51` |
| Revocation mid-session | `Hub.RevokeRoom` / `RevokeRoomForAll` | `internal/ws/room.go:226` |
| Search across workspaces and types | `search.TypeSpreadsheet` already exists as a declared type with no producer | `internal/search/doc.go:26` |
| Comments, activity, notifications | Drive + Work tracking (Phases 1–2) | ROADMAP §3b |
| Rate limiting the keystroke path | `collabRatePerSecond = 40`, `collabBurst = 120` | `internal/ws/client.go:60` |

**Phase 4 adds one migration, one table, three routes and one worker consumer.**
Everything else is client code and a formula engine. If an implementation of
this plan grows to five tables and twenty routes, it has rebuilt Drive inside
the spreadsheet and §3b has failed.

---

## 3. Data model

### 3.1 A spreadsheet is not a table

A spreadsheet is:

- a `files` row (Drive, Phase 1) with type `spreadsheet` — identity, ACL, name,
  folder, trash, versions;
- a `collab_documents` row with `resource_type = 'spreadsheet'` and
  `resource_id` = that file id — the `UNIQUE (resource_type, resource_id)`
  constraint at `migrations/015_collab.up.sql:61` is exactly this lookup;
- the cell data, inside the CRDT, in `collab_updates` + `collab_snapshots`.

There is no `sheets` table and no `cells` table. The server never parses a cell.

### 3.2 The one table: migration `017_spreadsheet.up.sql`

**Migration number.** 13 is used, 014 (SSO) and 015 (collab) are in flight, 016
is reserved for the remainder of that batch. This plan takes **017**. Phases 1–3
are being planned in parallel and will also claim numbers; this phase takes
exactly one migration in one file, so if Drive/work-tracking/docs land first
this becomes 020 or 021 with a rename and nothing else. Say so in the PR.

Search needs text out of a document the server cannot read. The established
answer in this codebase is to delegate to a client — that is already why
`TypeCollabCompact` exists and why `SendToRoomLeader` picks one connection to
produce a snapshot (`internal/ws/room.go:179`). The same mechanism produces a
search projection.

```sql
-- 017: The spreadsheet search projection.
--
-- The server cannot read a CRDT document, so it cannot extract the text of a
-- spreadsheet for the search index. A client that has the workbook open can,
-- and already does the equivalent for snapshots. This table is where that
-- client puts it.
--
-- It is derived state produced by an untrusted party, so three things are
-- structural rather than incidental:
--
-- 1. workspace_id is NOT taken from the request body. It is resolved from
--    collab_documents in the same statement. A client that could name its own
--    workspace could write a row that search then indexes into another tenant.
-- 2. source_seq is the collab log position the projection was computed from,
--    and an update with a lower or equal seq loses. Two clients in a room both
--    publish; without this the slower one's older text wins by arriving later.
-- 3. content is capped. It is a *digest* for search — tab names, header rows,
--    and display values truncated to fit — not a copy of the sheet. A 100k-cell
--    workbook must not become a 6 MB Meilisearch document.

CREATE TABLE sheet_projections (
    document_id  UUID PRIMARY KEY REFERENCES collab_documents(id) ON DELETE CASCADE,
    workspace_id UUID NOT NULL REFERENCES workspaces(id) ON DELETE CASCADE,
    source_seq   BIGINT NOT NULL,
    title        TEXT NOT NULL DEFAULT '',
    content      TEXT NOT NULL DEFAULT '',
    actor_id     UUID REFERENCES users(id) ON DELETE SET NULL,
    updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT sheet_projections_seq_positive  CHECK (source_seq >= 0),
    CONSTRAINT sheet_projections_title_size    CHECK (char_length(title) <= 512),
    CONSTRAINT sheet_projections_content_size  CHECK (octet_length(content) <= 65536)
);

-- Serves the cmd/reindex keyset walk directly: (updated_at, document_id) is a
-- total order for the same reason pkg/httputil.Cursor carries an id tiebreaker
-- (pkg/httputil/pagination.go:22) — a batch of projections published by one
-- transaction shares a timestamp.
CREATE INDEX idx_sheet_projections_walk ON sheet_projections (updated_at, document_id);
```

The write is a single compare-and-set, no lock dance:

```sql
INSERT INTO sheet_projections (document_id, workspace_id, source_seq, title, content, actor_id, updated_at)
SELECT d.id, d.workspace_id, $2, $3, $4, $5, NOW()
  FROM collab_documents d
 WHERE d.id = $1 AND d.resource_type = 'spreadsheet'
ON CONFLICT (document_id) DO UPDATE
   SET source_seq = EXCLUDED.source_seq, title = EXCLUDED.title,
       content = EXCLUDED.content, actor_id = EXCLUDED.actor_id, updated_at = NOW()
 WHERE sheet_projections.source_seq < EXCLUDED.source_seq
RETURNING workspace_id;
```

Zero rows returned means either "not a spreadsheet document" (404) or "stale"
(200, no-op) — distinguished by a cheap existence check, never collapsed.

### 3.3 The CRDT document shape

Not SQL, but it is the real data model and it is where the hard part lives, so
it is specified here rather than left to the implementer.

```
Y.Doc
  meta      : Y.Map     { title, calcTime, calcEpoch, schema }
  tabs      : Y.Array   [ tabId, ... ]                  -- order of the tab strip
  tab:<id>  : Y.Map     { name, rows: Y.Array<rowId>, cols: Y.Array<colId>,
                          cells: Y.Map<"rowId!colId", Y.Map>,
                          rowH: Y.Map<rowId,int>, colW: Y.Map<colId,int>,
                          freeze: {rows,cols} }
  cell      : Y.Map     { k: 'n'|'s'|'b'|'f'|'e',       -- kind
                          v: number|string|boolean,      -- literal, or cached
                                                         -- display value of a formula
                          f: SerializedFormula,          -- present iff k === 'f'
                          fmt: formatId }
```

Three decisions in that shape carry the whole phase:

**Cells are keyed by stable ids, never by A1.** `rows` and `cols` are Y.Arrays of
opaque ids; A1 is a *view*, computed from an id's index. This is what makes
"insert a row" not corrupt a concurrent edit — see §5.3.

**Formulas are stored in id space, not A1 text.** `SerializedFormula` is the AST
with every reference resolved to `{rowId, colId, rowRel, colRel}` and every range
to `{r0,c0,r1,c1}` of ids. The A1 text a user typed is regenerated for display.
See §5.3 for why this is not over-engineering.

**A formula cell caches its last computed display value in `v`.** The same thing
`.xlsx` does. It buys a first paint before recalculation finishes, it gives the
projection something to publish, and because it is derived it can be wrong
without being dangerous — the engine overwrites it on the next pass.

---

## 4. API surface

Three routes, following the existing conventions: `RegisterRoutes(mux, authMw)`
(`internal/file/handler.go:47`), `{data,meta,error}` envelope, keyset cursors,
`err != nil` → 500 and `!ok` → 403/404 never collapsed.

```go
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
	mux.Handle("PUT /api/v1/sheets/{document_id}/projection", authMw(http.HandlerFunc(h.PutProjection)))
	mux.Handle("GET /api/v1/sheets/{document_id}/projection", authMw(http.HandlerFunc(h.GetProjection)))
	mux.Handle("GET /api/v1/sheets/csv/{file_id}", authMw(http.HandlerFunc(h.ReadCSV)))
}
```

**`PUT /sheets/{document_id}/projection`** — a client with write capability
publishes the search digest.

```
{ "source_seq": 4211, "title": "Q3 forecast", "content": "Revenue\tPlan\t…" }
→ 200 { "data": { "indexed": true, "source_seq": 4211 } }
→ 200 { "data": { "indexed": false, "reason": "stale" } }   // lost the CAS
```

Authorization: `authz.Capability(subject, {spreadsheet, resource_id}) >= write`,
resolved from `collab_documents`, never from the URL or the body. On success it
publishes `superops.<workspace>.sheet.projected` with `PublishDurable`, which is
what the worker consumes.

**`GET /sheets/{document_id}/projection`** — read capability. Exists for the
verifier and for a debug view; it is also what an operator uses to confirm the
indexer is not the broken part.

**`GET /sheets/csv/{file_id}?cursor=&limit=`** — paged CSV read for import.

```
→ 200 { "data": { "rows": [["a","b"],["c","d"]] },
        "meta": { "cursor": "…", "has_more": true } }
```

Why the server parses a CSV the client could parse: the file is already in MinIO
behind `file.Storage`, a 50 MB CSV in a browser tab is a memory event, and the
client must stream it into the CRDT in chunks anyway (§8). Why the *client*
still does the writing: only the client can encode a Yjs update.

The cursor is a byte offset plus a row index, taken from
`csv.Reader.InputOffset()` — a record boundary the previous page proved is safe.
It is **not** an arbitrary byte offset: a quoted field may contain a newline, so
resuming at a guessed offset silently shifts every subsequent column. Encoded
with a local `encodeCSVCursor` rather than `httputil.EncodeCursor`, whose wire
format is `<time>|<id>` (`pkg/httputil/pagination.go:64`) and means something
else.

Authorization is `file.Handler.canRead`'s successor under Plan 00 — read
capability on the file object. `encoding/csv` from the standard library; no new
dependency.

**Deliberately absent:** create, list, rename, delete, share, export. Create is
`POST /api/v1/drive/files {type:"spreadsheet"}` plus the registry's
initialize-collab-document hook, and belongs to Phase 1. If Drive's create does
not take a type and a post-create hook, this phase adds a five-line
`POST /api/v1/sheets` shim and files the mismatch as seam debt rather than
building a parallel object lifecycle.

**Also absent: `GET /sheets/{id}/cells?range=A1:D20`.** It is the obvious seam
(a range embedded in a doc; a workflow reading a total) and it is the thing that
forces a second formula engine. §5.2 explains why it is refused, and §9 says
what it costs.

---

## 5. The hard part

The formula engine. Not "there is a parser" — the parser is a week. The hard
part is four coupled decisions, and getting any of them wrong makes the other
three unfixable.

### 5.1 Why cells are the easiest document model to make collaborative

The roadmap asserts it; here is the mechanism, because the whole engine design
follows from it.

A block editor's document is a **tree**, and merging two concurrent tree edits is
a structural problem: A splits a paragraph while B moves it; A wraps a range in a
list while B deletes half the range. Yjs solves it, but every *application-level*
invariant ("a list item's parent is a list") has to be re-established after every
merge, because the merged tree is one neither client authored.

A grid's document is a **map from a stable key to an independent value**. Two
clients editing different cells produce updates that touch disjoint keys — there
is no merge at all, only a union. Two clients editing the *same* cell produce
last-writer-wins on a register, which is not a compromise: a cell holds one
value, and there is no meaningful merge of `42` and `=SUM(A1:A5)`. There is no
sequence CRDT anywhere in the model except the two axis arrays.

The consequence that matters is stronger than "merging is easy":

> Every client converges to a byte-identical **input** map. If evaluation is
> deterministic, every client therefore converges to a byte-identical **output**
> without exchanging a single computed value.

That is the licence to keep the server CRDT-opaque (as `migrations/015` insists)
*and* to have no coordination protocol for recalculation. It is also fragile in
exactly one way, and §5.6 is about protecting it.

### 5.2 One engine, in TypeScript, in the client

The engine is TypeScript, lives in `app/src/lib/sheet/`, imports nothing from
React or React Native, and is unit-tested in plain node under the existing vitest
setup (`app/vitest.config.ts`).

**There is no Go engine.** The temptation is real: search wants text, export
wants values, a workflow wants a total. Every one of those is a reason to write
a second evaluator, and a second evaluator is the worst outcome available in this
phase — two implementations of ~72 functions and IEEE-754 display rounding and
Excel's date system, which must agree on every input forever, with no way to
detect disagreement except a user noticing that the number changed when they
opened the sheet.

What the refusal costs, stated plainly:

- A sheet nobody has open has a **stale search projection**. Bounded, visible
  (a verifier reports drift, §8), and repaired by the next person to open it.
- **CSV export is client-side.** The client has the values; the server does not.
- **No server-side range read.** Phase 7's "read a cell in a workflow" is not
  available.

The re-planning trigger: the first feature that genuinely needs authoritative
server-side values. At that point the honest move is to run *this same
TypeScript engine* headlessly — a small Node side-service, or QuickJS embedded —
not to reimplement it in Go. That is a deployment change (§3c shape: one
interface, transport chosen by config) and it is not this phase.

### 5.3 References, and why the CRDT binding is the second-hardest thing here

This is where the "cells are independent" story breaks, and it is the part most
spreadsheet implementations get wrong.

Key cells by A1 and insert a row: every address below the insertion shifts. A
concurrent edit to `A7` — encoded, sent, and merged after the insert — lands in
what is now the wrong cell. There is no CRDT that fixes this, because the CRDT
did exactly what it was told; the *addressing scheme* was wrong.

So: **rows and columns have opaque stable ids**, `rows`/`cols` are Y.Arrays of
those ids, and a cell key is `rowId!colId`. An insert is an insert into a Y.Array
— a sequence CRDT, which is the one thing Yjs is unambiguously good at. A
concurrent cell edit names ids, so it lands in the cell the author meant.

Formulas follow the same rule and get a large payoff for it:

```ts
type Ref   = { rowId: string; colId: string; rowRel: boolean; colRel: boolean }
type Range = { r0: string; c0: string; r1: string; c1: string }   // ids, inclusive
```

- **Insert/delete a row: no formula is rewritten.** Ids are stable, so every
  reference still points where the user pointed it. Compare the A1-text
  alternative, where inserting a row at the top of a 50k-formula sheet means
  rewriting 50k formulas — as one CRDT update that blows past both the 32 KiB
  socket payload cap (`internal/ws/client.go:39`) and the 1 MiB row constraint
  (`migrations/015_collab.up.sql:86`), and which two clients doing it
  concurrently would each produce a different version of.
- **Delete a referenced row:** the id is absent from `rows`, so resolution fails
  at evaluation time and the cell yields `#REF!`. No rewrite, no scan, no
  "which formulas pointed at row 12" query.
- **A range grows correctly.** `A1:A10` is `{r0: id_1, r1: id_10}`, resolved as
  "every row currently between those two in `rows`". Insert a row inside it and
  it is in the range — which is the behaviour a user expects and which
  index-based ranges only get by rewriting.
- **Relative vs absolute is a copy-time property, not a storage property.**
  `rowRel`/`colRel` are consulted when a formula is filled or pasted: the delta
  is computed in current index space, and the new target ids are looked up.
  Storing offsets instead is the obvious alternative and it is wrong — `C5 = A3`
  stored as offset `(-2,-2)` points at `A4` after a row is inserted between
  them, and Excel's semantics are that the reference follows the referenced cell.

Cross-tab references carry a `tabId` on the same footing.

The cost is a real serialization layer: A1 text ⇄ id AST on every edit, and A1
rendering on every display of a formula. Budget it as its own module (`refs.ts`)
with its own property tests, because it is the one place where a bug is silent
and permanent.

### 5.4 Grammar and parser

Hand-written lexer plus a precedence-climbing recursive-descent parser. No
parser generator, no dependency.

```ebnf
formula        := '=' comparison
comparison     := concat      { ('=' | '<>' | '<' | '<=' | '>' | '>=') concat }
concat         := additive    { '&' additive }
additive       := multiplicative { ('+' | '-') multiplicative }
multiplicative := exponent    { ('*' | '/') exponent }
exponent       := unary       { '^' unary }          (* LEFT-assoc: 2^3^2 = 64 *)
unary          := { '-' | '+' } postfix
postfix        := primary { '%' }
primary        := NUMBER | STRING | BOOL | ERRORLIT
                | reference | call | '(' comparison ')'
call           := IDENT '(' [ comparison { ',' comparison } ] ')'
reference      := [ sheet '!' ] ( range | cell | NAME )
range          := cell ':' cell | colref ':' colref | rowref ':' rowref
cell           := [ '$' ] COLLETTERS [ '$' ] ROWDIGITS
sheet          := IDENT | "'" ... "'"
```

The left-associative `^` is not a typo. It is the spec of this engine in one
line: **match a spreadsheet, not match mathematics.** `2^3^2` is 64 in Excel and
Google Sheets and 512 everywhere else; a user pasting a formula in expects the
first answer.

Not parsed, on purpose: array literals `{1,2;3,4}`, the intersection operator
(space), union outside a function, R1C1 mode, 3-D refs (`Sheet1:Sheet3!A1`),
structured/table references.

The AST carries source spans so a parse error can be underlined in the formula
bar at the offending character instead of reported as "invalid formula".

**Values** are a five-member lattice: `number | string | boolean | error | blank`.
Blank is not zero and not `""`, and coerces to both — `SUM(A1)` on a blank is 0,
`LEN(A1)` is 0, `A1=0` is TRUE, `ISBLANK(A1)` is TRUE. Errors are values that
propagate through arithmetic and are trapped only by `IFERROR`/`IFNA`/`ISERROR`.
Dates are numbers with a format, serial days from 1899-12-30, 1900 date system
only — **including the Lotus leap-year bug** (serial 60 is "1900-02-29", a day
that did not exist). Reproduce it. Every CSV and XLSX in the world round-trips
through it, and being right where Excel is wrong is a compatibility defect.

Numbers are IEEE-754 doubles with display rounding to 15 significant digits,
which is what Excel does and is why `0.1+0.2` shows `0.3` there. No decimal
library, no dependency.

**~72 functions**, inside the roadmap's 50–80 guard:

| Group | Functions |
|---|---|
| Math (14) | SUM SUMIF SUMIFS PRODUCT ABS ROUND ROUNDUP ROUNDDOWN INT MOD POWER SQRT CEILING FLOOR |
| Stats (11) | AVERAGE AVERAGEIF COUNT COUNTA COUNTBLANK COUNTIF COUNTIFS MIN MAX MEDIAN STDEV |
| Logic (8) | IF IFS IFERROR IFNA AND OR NOT SWITCH |
| Text (16) | CONCAT CONCATENATE TEXTJOIN LEFT RIGHT MID LEN LOWER UPPER PROPER TRIM SUBSTITUTE REPLACE FIND SEARCH TEXT VALUE |
| Lookup (6) | VLOOKUP HLOOKUP INDEX MATCH XLOOKUP CHOOSE |
| Date (12) | DATE TODAY NOW YEAR MONTH DAY HOUR MINUTE SECOND EOMONTH DATEDIF NETWORKDAYS |
| Info (5) | ISBLANK ISNUMBER ISTEXT ISERROR ISNA |

`INDIRECT` and `OFFSET` are absent and their absence is load-bearing: **the
dependency graph is a pure function of the parsed formulas.** A cell's precedents
are readable from its AST without evaluating anything. `INDIRECT("A"&B1)` makes
the edge set depend on a *value*, which means the graph must be re-derived
mid-recalculation, which means the topological order can change while you are
walking it, which means cycle detection is no longer total. Two functions are
not worth surrendering that.

### 5.5 The dependency graph, and recalculating 100k cells without freezing

**The graph is derived, in memory, and never stored.** Not in the CRDT, not in
Postgres. It is a pure function of the cell map, so storing it would be caching
derived state in a document two clients can concurrently mutate — a merge
problem invented for no reason. Building it on load is one pass over the
formula cells.

Two structures:

```ts
precedents: Map<CellKey, Ref[] | Range[]>   // from the AST, computed at parse
dependents: Map<CellKey, Set<CellKey>>      // reverse index, maintained incrementally
```

**Ranges are the scaling trap.** `=SUM(A:A)` naively registers one edge per cell
in the column. A thousand such formulas is a hundred million edges. Instead,
range dependents go in a **block index**: rows are bucketed in blocks of 64 per
(tab, column), and a range registers itself against every block it covers. An
edit to a cell looks up its block, gets the candidate ranges, and filters them by
actual containment. `SUM(A:A)` costs `ceil(rows/64)` entries, an edit costs one
lookup plus a short filter, and the memory is bounded by the number of range
references rather than by their area.

**Editing a cell** (`refs.ts` → AST → precedents diff):
1. Remove the old edges, add the new ones. `O(refs in this formula)`.
2. Mark the cell dirty; BFS over `dependents` marking the transitive closure
   dirty. Bounded by the size of the affected subgraph, not the sheet.
3. Order the dirty set topologically and evaluate.

**Topological order + cycle detection in one pass.** Iterative DFS with an
explicit stack and three-colour marking (white / grey / black) over the dirty
subgraph only. Recursion is not an option: chain depth is user-controlled and a
50k-deep chain blows the JS stack, in a way that surfaces as a browser tab
crash rather than an error. Reaching a **grey** node is a cycle; every node on
the stack from that grey node upward is a member of it, and they all resolve to
`#REF!` with a `circular: [cellKeys]` detail so the UI can name the loop and
offer to jump to it. Cells *downstream* of a cycle also get `#REF!` by normal
error propagation. Excel's iterative-calculation mode (converge a cycle over N
passes) is **cut**.

**The 8 ms budget.** Evaluation runs in a cooperative scheduler: evaluate cells
in topological order until the frame budget is spent, then yield via
`MessageChannel` and resume on the next tick. Because the order is computed
before any evaluation begins, a partial pass is always *consistent* — a cell is
only ever evaluated after all its precedents, so the values on screen are a
prefix of the final state, never a mixture. Cells not yet reached render their
cached `v` with a "calculating" affordance. Dirtying a cell mid-pass merges into
the current dirty set and re-sorts only the affected region.

**Not a Web Worker.** It is the obvious suggestion and it is wrong here: the Yjs
document lives on the main thread, so a worker needs either a second copy of the
cell map kept in sync (a sync problem strictly worse than the scheduling problem
it solves) or `SharedArrayBuffer` and cross-origin isolation headers, which the
deployment does not have. Chunked cooperative scheduling gets a responsive UI
with one copy of the state. Worker offloading is a later optimization, and a
web-only one — ROADMAP §3 has mobile read-only anyway.

Rough budget to hold the design to: a 100k-cell sheet of which 30k are formulas
should build its graph in under 300 ms and complete a cold full recalculation in
under 1.5 s of wall time at ≤ 8 ms per chunk; a typical single-cell edit dirties
tens of cells and completes inside one frame. These are CI gates, not aspirations
(§10).

### 5.6 Determinism is the invariant, and two functions break it

§5.1's convergence argument requires that evaluation be a pure function of the
cell map. `TODAY()`, `NOW()`, and `RAND()` are not.

Two clients with an identical document showing different numbers is the worst
class of bug this phase can ship: both values are plausible, neither user has any
reason to doubt theirs, and there is no error state.

- `RAND`, `RANDBETWEEN`: **cut.** A document seed would make them converge, but
  the resulting UX (your neighbour's edit silently changes your random numbers)
  is not worth it in v1.
- `TODAY`, `NOW`: evaluated against `meta.calcTime`, a timestamp **in the CRDT**.
  Volatile cells are dirtied when `calcTime` changes; `calcTime` advances when a
  client opens the workbook, when a user hits "recalculate", and hourly from
  whichever client the room leader logic already picks
  (`Hub.SendToRoomLeader`, `internal/ws/room.go:179`). Every client evaluates
  against the same instant, so they agree; the instant is LWW like any other
  value, so they converge.

And a safety net for the general case, which costs nearly nothing because the
mechanism exists: each client folds a **rolling hash of its computed values per
tab into its awareness payload**. Awareness is already ephemeral, already
relayed, and already never persisted (`CollabAwarenessData`,
`internal/ws/protocol.go:134`). If two clients in a room publish different
digests at the same `head_seq`, that is a determinism bug — the UI shows
"recalculating", forces a full recalculation, and the client reports it. This
turns the phase's most dangerous silent failure into a loud one, in about thirty
lines.

---

## 6. Package layout

**Backend** — one new package.

```
internal/sheet/
  handler.go       PutProjection, GetProjection, ReadCSV; RegisterRoutes(mux, authMw)
  repository.go    the projection CAS upsert and read
  projection.go    normalisation + validation of a published digest
  csv.go           resumable paged read over a MinIO object (encoding/csv)
```

Reuses, does not rebuild: `internal/authz` (capability), `internal/collab`
(document lookup and workspace resolution), `internal/file.Storage` (the CSV
source), `pkg/httputil` (envelope, cursors, `DecodeJSON`), `pkg/database.WithTx`.

**Edits to existing packages, all small:**

- `internal/search/doc.go`: add `SpreadsheetDoc` next to `MessageDoc` and
  `FileDoc` (`internal/search/doc.go:270`, `:311`). `Type: TypeSpreadsheet`
  already exists (`:26`). Its ACL comes from the object's access keys under Plan
  00 — never from the publishing client — and `Doc.validate()` (`:212`) already
  refuses an empty key set as a permanent error, which is the fail-closed
  behaviour this needs.
- `cmd/worker/main.go`: one more `bind(durableSpec{...})` alongside the existing
  five, `durable: "indexer-sheet"`, `filter: "superops.*.sheet.projected"`.
  Plus a projection-drift verifier in the job-loop set (§8), in the same shape as
  the existing retention and object-GC loops.
- `cmd/reindex/main.go`: a spreadsheet source walking `sheet_projections` in
  `(updated_at, document_id)` keyset order, exactly like the message walk.
- `internal/app/app.go`: register the handler under the existing conventions.

**No new Go dependencies.** `encoding/csv` is stdlib. (XLSX would require
`github.com/xuri/excelize/v2`; that is why XLSX is cut.)

**Client** — the bulk of the phase.

```
app/src/lib/sheet/            engine — zero React, zero react-native imports
  lexer.ts  parser.ts  ast.ts        grammar (§5.4)
  refs.ts                            A1 ⇄ stable-id, fill/paste transposition (§5.3)
  values.ts                          the value lattice, coercion, errors, dates
  functions/*.ts                     ~72 functions, one file per group
  graph.ts                           dependents index + range block index (§5.5)
  scheduler.ts                       chunked topological evaluation
  engine.ts                          façade: applyEdits(), valueAt(), digest()
  format.ts                          number/date display formats
  csv.ts                             parse (client side of import) + serialize (export)
  doc.ts                             the ONLY file that imports yjs
app/src/components/sheet/
  Grid.tsx  Cell.tsx  FormulaBar.tsx  SelectionLayer.tsx  ColumnHeader.tsx
app/src/screens/SheetScreen.tsx
app/src/api/sheets.ts
```

`app/src/lib/sheet/**` importing nothing from React Native is a hard rule, not
a preference: it is what lets the entire engine run in node under vitest, which
is the only affordable way to test a 100k-cell recalculation.

**No new client dependencies.** `yjs` and `y-protocols` arrive with Phase 0. The
grid virtualizer is written here (~300 lines of windowed absolute positioning);
every off-the-shelf virtualizer fights `react-native-web`, and a grid needs
two-axis windowing with frozen panes that none of them do well anyway.

---

## 7. Sequencing

```
  A  engine core        ── parser, values, evaluator, first 20 functions   [long pole, starts day 1]
  B  graph + scheduler  ── depends on A's AST                             [highest risk]
  C  function library   ── parallel with B, one test per function          [continuous]
  D  grid + interaction ── fully parallel with A/B/C, different person     [second longest]
  E  Yjs binding        ── needs Phase 0 collab; developable vs a local Y.Doc
  F  backend            ── migration, internal/sheet, search, worker, reindex   [S]
  G  CSV import         ── needs F's endpoint and A's csv.ts               [last, S]
```

**Ships first:** A. It has no dependency on Phase 0, Phase 1, the grid, or the
network — it is a pure function from a cell map to a value map, and it can be
correct and benchmarked before anything renders. Starting anywhere else means
discovering the reference model (§5.3) after the grid has been written against
A1 addressing.

**Parallel:** D from day one (it renders a static fixture until E exists), C from
the moment A's function-call protocol is stable, F from the moment the collab
document lookup exists.

**Long pole:** A + B. Not because either is enormous, but because they are
serial, they are where every subtle correctness bug lives, and every other track
eventually waits on them.

**First demoable milestone:** A + B + D against a fixture — a single-user
spreadsheet with real formulas and no persistence. That is the point at which
the phase's risk is retired; everything after it is integration.

---

## 8. Risks and failure modes

**Two engines.** Ranked first because it is the only failure that cannot be
walked back. Guard: no Go evaluator, and no route that would require one (§4).
The pressure will arrive as an innocuous request ("just let the workflow read a
total"). Treat it as a re-planning trigger.

**Silent divergence between clients.** Two people, same document, different
numbers. Guards: determinism as an explicit invariant (§5.6), the awareness value
digest, and the property test that says incremental recalculation equals a
recalculation from scratch (§10).

**Update-log growth.** A cell edit must be one CRDT update, not one per
keystroke — write on commit, not on change. Otherwise `collab_updates` grows by
a row per character and the snapshot compaction the collab layer already
implements never keeps up.

**Bulk paste and import exceed the transport.** A 50k-cell paste is one logical
transaction but far more than the 32 KiB socket payload cap
(`internal/ws/client.go:39`) and past the 1 MiB row constraint
(`migrations/015_collab.up.sql:86`). Bulk writes are chunked into ≤ 256 KiB
updates, applied via the HTTP append path, with a progress indicator. This is
not an optimization; without it the first real import fails.

**Snapshot ceiling.** `collab_snapshots.payload` is capped at 16 MiB
(`migrations/015_collab.up.sql:99`). A 100k-cell workbook with formulas encodes
to roughly 5–8 MB of Yjs state — the same order as the cap. **The v1 workbook
budget is 250k cells or 10 MiB of encoded state**, enforced client-side on
paste and import with a clear message. This limit exists because of that
constraint, and raising it is a collab-layer conversation, not a spreadsheet
one.

**Memory, and mobile.** 100k cells with parsed ASTs is 50–100 MB of JS heap.
Fine on desktop web (ROADMAP §3 recommendation (1)); not fine on a phone.
Mobile opens read-only and refuses workbooks over ~20k cells with "open on
desktop". Blunt, and better than an OOM.

**Projection staleness and drift.** The digest is only as fresh as the last
client to have the sheet open. Guard: a worker job comparing
`sheet_projections.source_seq` against `collab_documents.head_seq` and reporting
the distribution of the gap — the same shape as Plan 00's `acl_key` drift
verifier, and for the same reason: silently divergent derived state is a bug you
only learn about from a user.

**Projection as an injection vector.** The content is authored by a client. The
handler must resolve `workspace_id` from `collab_documents` in the same
statement (§3.2), take ACL keys from `authz`, and cap length in the database as
well as in the handler. A client that could name its own workspace could write
into another tenant's search results.

**Range-dependency blowup.** `=SUM(A:A)` filled down a thousand rows. Guarded by
the block index (§5.5), which must be in place before the first benchmark, not
retrofitted after someone's sheet locks their browser.

**Deep or wide dependency chains at load.** A cold open of a large sheet is
decode + graph build + full recalculation. Guard: paint from cached `v` values
immediately, recalculate in the background, and make "cold open to first paint"
a measured number.

**CSV paging across a quoted newline.** The bug this endpoint will have. Covered
by an integration test with a fixture designed to straddle a page boundary
inside a quoted field (§10).

**CI cannot currently catch any of this.** `.github/workflows/ci.yml` runs
`npm run typecheck` and `npm audit` for the app and **never runs `npm test`** —
the vitest suite exists and CI ignores it. The entire correctness argument for
this phase lives in that suite. Adding `npm test` to the `app` job is a
prerequisite of the phase, not a nice-to-have. The integration job also runs
without Meilisearch (`MEILI_HOST: ''`), so the projection → index path would be
skipped; adding a Meilisearch service container is the second prerequisite.

---

## 9. Gaps in Plan 00 this phase runs into

Stated as gaps rather than worked around, per the instruction in Plan 00.

**Sub-object capability.** Plan 00 grants `(subject, object, capability)` with
capability ordered `admin > share > write > comment > read`. A spreadsheet's
natural next request is "this person may edit column D and nothing else"
(protected ranges), which is a capability scoped to a *part* of an object. Plan
00 has no such concept, and inventing one inside `internal/sheet` is exactly the
per-pillar permission model §1 warns about. **Protected ranges are cut for v1**,
and if they become a requirement the design belongs in Plan 00 — likely as a
capability qualified by an opaque, editor-defined scope string that the ACL layer
stores and compares but never interprets.

**Capability for a room that is already open.** `RoomAccess.CanWrite` is captured
at join (`internal/ws/room.go:51`) and the hub has `RevokeRoom`. A *downgrade*
from write to read is not a revocation — the user should stay in the room and
lose the ability to append. The existing `Recheck` on `RoomHandler`
(`internal/ws/room.go:65`) is the right hook; confirm the collab layer calls it
on a grant change rather than only on the 5-minute backstop
(`membershipRecheckPeriod`, `internal/ws/client.go:64`).

---

## 10. Verification

**Engine — vitest, `app/test/sheet/`.** This is where the phase's correctness
lives.

- **A golden corpus as the oracle.** A committed fixture of
  `formula | inputs | expected`, thousands of rows, generated once by running the
  same cases through LibreOffice headless (`soffice --headless --convert-to csv`)
  and checked in with the generator script. Excel semantics are not guessable —
  `=1/0`, `=""&1`, `="1"+1`, `=TRUE+1`, `=DATE(1900,2,29)`, `=2^3^2` — and a
  real oracle is cheaper than a hundred arguments about what is correct.
- **Property: order independence.** Apply a random permutation of the same edit
  set; final values must be identical. Catches evaluation-order bugs directly.
- **Property: incremental == full.** Random dependency graphs, random edits;
  the incrementally maintained state must equal a recalculation from scratch.
  **The single highest-value test in the phase** — it is the assertion that the
  dirty-propagation logic in §5.5 is sound.
- **Property: convergence.** Two in-memory `Y.Doc`s, two random concurrent edit
  streams *including row and column insert/delete*, synced; both engines must
  produce identical values and identical A1 renderings of every formula. This is
  the test that would catch a §5.3 regression.
- **Cycles:** self-reference, two-cycle, long cycle, cycle reachable only through
  a range, cell downstream of a cycle. All `#REF!` with the correct membership
  list, no hang, no stack overflow.
- **Perf gates, failing the build on regression:** 100k cells / 30k formulas —
  graph build < 300 ms, cold full recalculation < 1.5 s, single-cell edit at the
  root of a 50k chain < 250 ms, and **no scheduler chunk over 16 ms**. The last
  one is the UI-responsiveness assertion and the one most likely to regress.

**Integration suite — `backend/test/integration/sheet_test.go`**, against the
real Postgres/NATS/Meilisearch/MinIO the suite already uses, reusing
`harness.newTenant` (`test/integration/harness_test.go:468`),
`harness.dialWS` (`:659`) and `harness.searchHits` (`:609`).

- `TestSheetProjectionIndexed` — publish a projection, wait for the
  `indexer-sheet` consumer, assert the sheet appears in unified search for a
  member. Modelled on `TestCrossTenantSearch` (`test/integration/tenancy_test.go:30`).
- `TestSheetProjectionIsWorkspaceScoped` — tenant B never sees tenant A's sheet,
  and a body claiming A's `workspace_id` does not change that. The
  cross-tenant regression this table's design exists to prevent.
- `TestSheetProjectionRequiresWrite` — reader → 403, non-member → 404, and a
  database error → 500 and not 403 (the `(bool, error)` rule at
  `internal/authz/authz.go:12`).
- `TestSheetProjectionStaleSeqLoses` — publish seq 100 then seq 50; the row still
  reads 100 and the response says `indexed: false`.
- `TestSheetCSVPagingIsTotal` — a fixture whose quoted, newline-containing field
  straddles a page boundary; paging the whole file must reconstruct it exactly.
  Sibling of `TestPaginationIsTotal` (`test/integration/integration_test.go:181`).
- `TestSheetCSVAuthz` — a CSV in a workspace the caller does not belong to is a
  404, not a 403 that confirms the file exists.
- `TestSheetRoomWriteRevocation` — two sockets in the same room; downgrade one to
  read; its `collab.update` frames are rejected and the other client's are not.
  Extends `TestWebSocketRevocation` (`test/integration/realtime_test.go:150`).

**CI prerequisites** (from §8): add `npm test` to the `app` job and a
Meilisearch service container to the `integration` job. Without the first, the
engine's entire test suite is decorative.

---

## 11. Sizing

| Piece | Size | Notes |
|---|---|---|
| Parser, values, coercion, dates | M | Bounded, well-specified, high test density |
| Function library (~72) | M | Wide, shallow, parallelizable, one test each |
| Dependency graph + scheduler | M | Smallest of the engine pieces; **highest risk** |
| Reference model + Yjs binding | M | §5.3 — a bug here is silent and permanent |
| Grid + interaction (virtualization, selection, fill, clipboard, formats) | L | Fully parallel; well-trodden but genuinely large |
| Backend (migration, `internal/sheet`, search, worker, reindex) | S | Three routes, one table |
| CSV import/export | S | |
| **Phase total** | **L** | Matches ROADMAP §5 |

**Long pole:** the engine core plus the graph and scheduler — items 1, 3 and 4.
They are serial with each other, they gate the binding, and they are where the
bugs that survive to production live. The grid is the larger number of hours and
the smaller share of the risk.
