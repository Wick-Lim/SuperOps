# Plan 06 — the design surface

**Phase 5. The third editor, and the heaviest client build in the product.**

Depends on: Drive + the editor registry (§3b), the collaboration layer
(`migrations/015_collab.up.sql`, `internal/ws/room.go`), object-level
permissions (`docs/plans/00-permissions.md`). It adds almost nothing to the
backend — one migration, four routes — and that asymmetry is the point: if this
phase needs a fifth backend subsystem, the substrate was built wrong.

Status: design. Not started.

---

## 1. What it is

A design file opens on an infinite canvas. You draw frames, rectangles,
ellipses, lines and text; you drop in images from Drive; you nest things inside
frames, arrange them with a row/column layout, align and snap them against each
other; you turn a subtree into a reusable component and place instances of it
with per-instance overrides. Several people do all of that at once, seeing each
other's cursors, selections and in-flight drags. The file lives in Drive, so it
inherits sharing, permissions, versions, comments, activity and search from
Phase 1 — this plan does not build any of those.

The object vocabulary, stated completely, because "load-bearing scope guards"
means the list is the specification:

| Node type | Properties |
|---|---|
| `frame` | transform, fill, stroke, corner radius, opacity, clips-children, optional `layout` (row/column) |
| `rect` | transform, fill, stroke, corner radius, opacity, shadow |
| `ellipse` | transform, fill, stroke, opacity, shadow |
| `line` | transform (two endpoints), stroke, caps |
| `text` | transform, Y.Text content, font family/size/weight/style, line height, letter spacing, colour, align, auto-width / auto-height / fixed |
| `image` | transform, Drive file id, fit (cover/contain/fill), corner radius, opacity |
| `group` | transform, children (no fill of its own) |
| `instance` | component id, transform, overrides (text content, fills, visibility) |

Fills are a solid colour, a linear gradient, or an image. One stroke per node
(width, colour, alignment inside/center/outside, dash). One drop shadow. That
is the whole paint model, and it is exactly what Canvas2D gives for free.

### Not in scope

The five re-planning triggers from ROADMAP §5/§7 — **pen tool, boolean path
operations, plugin runtime, prototyping engine, design-system tooling** — priced
in §16 so that a future request to add one is a decision rather than a backlog
grooming. Additionally cut from v1:

- **Multi-page files.** Frames on an infinite canvas cover it. Adding pages
  later is S.
- **Constraints / responsive resize.** Absolute positioning plus the row/column
  layout; ROADMAP §5 already dropped the constraint solver.
- **Server-side export and rendering.** Export is client-side (PNG via the same
  canvas, SVG via a display-list serializer — see §6). No headless browser in
  the worker.
- **Custom font upload.** A bundled, self-hosted set only (§7). Font upload
  means font parsing, serving, and per-client availability skew.
- **Nested components** (an instance inside a main component) and cross-file
  component libraries.
- **Blend modes, blurs, multiple fills/strokes per node, vector networks.**
- **Native (iOS/Android) editing.** ROADMAP §3 option 1: web edits, native
  shows the rendered preview, comments and file metadata. This is the pillar
  the recommendation was written for.

---

## 2. What this phase assumes, and the one gap it cannot close itself

Assumed present, with the code that already anticipates it:

| Needed | Already exists |
|---|---|
| Object storage + upload/download | `internal/file` + MinIO; `file.Handler.RegisterRoutes` at `internal/file/handler.go:47` |
| CRDT log, snapshots, compaction | `migrations/015_collab.up.sql`; `collab_documents.resource_type` is validated by shape (`:62`), so `'design'` needs no migration |
| Room transport, revocation, leader election | `internal/ws/room.go` — `BroadcastToRoom:127`, `SendToRoomLeader:179`, `RevokeRoom:226`, `RoomMemberIDs:197` |
| Search over designs | `search.TypeDesign` already declared at `internal/search/doc.go:28`; access keys at `:105-122` |
| Object ACLs | `docs/plans/00-permissions.md` — `Capability`, `KeysFor` |

**The gap: a file that belongs to an object rather than to a message.**

`file.Handler.canRead` (`internal/file/handler.go:251`) authorizes an unattached
file to *its uploader only*, and the orphan collector deletes every file with
`message_id IS NULL` older than the grace period —
`internal/file/repository.go:54`, called from `runObjectGC` at
`cmd/worker/main.go:1121`. As written today, an image dropped onto a shared
design is invisible to every collaborator and then permanently deleted a day
later.

That is Drive's problem to solve, not this phase's, and plan 00 says to name the
gap rather than route around it. Concretely, what Drive/permissions must
provide: **a file may be parented to a non-message object, its effective
capability is that object's capability, and the orphan collector treats a
parented file as attached.** In plan-00 terms the asset file gets an `acl_object`
row whose `path` sits under the design's path, and `Capability(subject,
file)` inherits.

If Drive has not shipped that when this phase starts, §3 contains the fallback:
a `design_assets` join table and a `canRead` branch, ~80 lines, deleted when
Drive catches up. Either way, §14 has the integration test that fails loudly if
the GC eats an asset.

---

## 3. Data model

**Almost nothing.** The document itself is opaque CRDT state in
`collab_updates` / `collab_snapshots`, keyed `('design', <drive file id>)` by
`collab_documents_resource_key`. The scene graph is never in Postgres, is never
parsed by Go, and has no schema here — that is the whole bet of the
collaboration layer and this plan does not hedge it.

What Postgres must know: which files are assets of which design, and where the
rendered preview and the text digest are.

### Migration `022_design.up.sql`

I take **022**, assuming Drive takes 017, work tracking 018–019, docs 020,
spreadsheet 021 (in-flight 014–016). Nothing in this plan depends on the number;
renumbering is mechanical. It must land after Drive's file changes and after
015.

```sql
-- Assets: images referenced by a design. FALLBACK ONLY — delete this table the
-- moment Drive can parent a file to an arbitrary object (see §2).
CREATE TABLE design_assets (
    design_id  UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    file_id    UUID NOT NULL REFERENCES files(id) ON DELETE CASCADE,
    created_by UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (design_id, file_id)
);
-- The GC-safety lookup: "is this file an asset of something?" must be one
-- indexed probe, because the orphan sweep runs it in bulk.
CREATE UNIQUE INDEX idx_design_assets_file ON design_assets (file_id);
-- Keyset pagination for the assets panel.
CREATE INDEX idx_design_assets_listing ON design_assets (design_id, created_at DESC, file_id DESC);

-- Derived state a client produced, because the server cannot render or read a
-- CRDT document. One row per design; both fields are replaceable at any time.
CREATE TABLE design_derived (
    design_id     UUID PRIMARY KEY REFERENCES files(id) ON DELETE CASCADE,
    -- The preview image, stored as an ordinary file row (and therefore also an
    -- asset for GC purposes).
    preview_file_id UUID REFERENCES files(id) ON DELETE SET NULL,
    -- Plain text extracted from text layers and node names, for the search
    -- index. Client-supplied and therefore bounded and untrusted (§13).
    text_digest   TEXT NOT NULL DEFAULT '',
    -- The collab log position this derived state was computed from. A late
    -- upload computed from an older state must not overwrite a newer one.
    source_seq    BIGINT NOT NULL DEFAULT 0,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    CONSTRAINT design_derived_digest_size CHECK (octet_length(text_digest) <= 65536),
    CONSTRAINT design_derived_seq_positive CHECK (source_seq >= 0)
);
```

`source_seq` is the whole concurrency story for derived state: every write is
`... WHERE source_seq < $new`, so two clients racing to upload a preview leave
the one computed from the later state, and a slow client that finishes after a
newer upload is discarded rather than regressing the preview. Same idea as the
compaction guard in 015, for the same reason.

A design file itself is a Drive file with
`content_type = 'application/vnd.superops.design'`. The editor registry
dispatches on it. No new table.

---

## 4. API surface

Four routes. Everything else — open, load state, append updates, snapshot,
awareness, comments, sharing, versions, search — is Drive's or collab's.

```go
// internal/design/handler.go
func (h *Handler) RegisterRoutes(mux *http.ServeMux, authMw func(http.Handler) http.Handler) {
    mux.Handle("POST /api/v1/designs/{design_id}/assets",  authMw(http.HandlerFunc(h.UploadAsset)))
    mux.Handle("GET  /api/v1/designs/{design_id}/assets",  authMw(http.HandlerFunc(h.ListAssets)))
    mux.Handle("PUT  /api/v1/designs/{design_id}/preview", authMw(http.HandlerFunc(h.PutPreview)))
    mux.Handle("PUT  /api/v1/designs/{design_id}/digest",  authMw(http.HandlerFunc(h.PutDigest)))
}
```

- `POST .../assets` — multipart, same shape and limits as
  `file.Handler.Upload` (`internal/file/handler.go:139`): `MaxBytesReader`,
  1 MiB multipart memory so the body spills to disk, server-side content sniffing
  (`sniffContentType:95`), and it **reuses that handler's storage path rather
  than reimplementing it**. Requires `write` on the design. Rejects anything
  that is not a raster image type. Returns `{id, name, content_type, width,
  height, size_bytes}` — `files.width/height` already exist
  (`migrations/004_create_messages.up.sql:41`) and are populated here so the
  editor can place the image at intrinsic size without decoding it first.
- `GET .../assets` — `httputil.JSONList` with a keyset cursor over
  `(created_at, file_id)`, exactly like the bookmark listing at
  `internal/message/handler.go:945`. Requires `read`.
- `PUT .../preview` — multipart image, ≤512 KiB, ≤2048px; body carries
  `source_seq`. Requires `write` (a read-only viewer must not be able to change
  what everyone else sees in Drive). Idempotent, last-seq-wins.
- `PUT .../digest` — `{"text": "...", "source_seq": N}`, ≤64 KiB, requires
  `write`. Publishes `superops.design.indexed` so the worker's existing search
  indexer writes a `search.Doc{Type: TypeDesign}` — the ACL on that doc is
  computed server-side from the design object's access keys, never from
  anything the client sent.

Standard conventions throughout: `{data, meta, error}` via `pkg/httputil`,
`err != nil` → 500 and `!ok` → 403/404 never collapsed, authorization as the
first statement of the handler body.

**Routes deliberately absent:** no server-side export, no component-library
endpoints, no font endpoints, no "render this design" endpoint. Each of those is
a subsystem wearing a route.

---

## 5. Package layout

### Backend — small on purpose

```
internal/design/
  handler.go      4 routes above; authz via the object Capability check
  repository.go   design_assets + design_derived; keyset listing
  index.go        DesignDoc -> search.Doc (mirrors search.FileDoc at doc.go:311)
```

Reused, not rebuilt: `internal/file.Storage` (upload/download/delete),
`internal/authz` (+ plan 00's `Capability`), `internal/search` (typed doc +
access keys), `internal/collab` (document lifecycle, compaction),
`internal/ws` (room transport, revocation), `cmd/worker`'s indexer loop.

Two edits outside the package: register in `internal/app/app.go` beside the file
handler (`app.go:337`), and teach the orphan collector that a parented file is
not an orphan (`internal/file/repository.go:54`).

### Client — where the phase actually lives

```
app/src/design/
  model/      node types; the Yjs binding; normalize() (§9); ops; undo; clipboard
  layout/     transform resolution; auto-layout; text measurement + cache
  render/     display-list builder; Canvas2D painter; culling; dirty rects; overlay
  interact/   pointer state machine; snapping; keyboard; zoom/pan
  ui/         toolbar, layers panel, properties panel, assets panel, comment pins
  index.web.tsx     mounts the editor
  index.native.tsx  mounts preview + comments, read-only
```

The `.web/.native` split is React Native's platform-extension resolution and is
the seam that keeps `<canvas>`, `document.fonts` and pointer events out of the
native bundle entirely — no runtime `Platform.OS` branches in the renderer, and
the native app never pays the bundle cost of an editor it cannot run.

**New dependencies: none.** `yjs` arrives with the collaboration layer, not with
this phase. The spatial index is a uniform grid (~150 lines) rather than
`rbush`; pointer handling is raw DOM pointer events rather than another gesture
library; the fonts are static `.woff2` assets. If a reviewer wants a dependency
here, the argument to beat is that each one is a supply-chain and upgrade cost
against ~150 lines of code we would have to understand anyway.

---

## 6. Renderer: Canvas2D, retained scene graph, DOM only for text editing

Three candidates, at the object counts this scope implies (typical 200–2,000
nodes, design ceiling 10,000):

**DOM/SVG.** Every node is an element; pan/zoom is one transform on the root
`<g>` and is GPU-composited and crisp; hit testing comes free from the browser;
it fits React's model. It loses on three counts. Performance past a few thousand
elements is browser-specific and cliff-shaped — style recalc and layout on a
tree that large is not something you can profile your way out of. Text that
wraps needs `foreignObject`, and an SVG containing `foreignObject` cannot be
reliably rasterized to a canvas, which breaks preview generation (§13) and
would force a second renderer for it. And the "free hit testing" is worth less
than it looks: marquee selection, snap-candidate lookup and viewport culling all
need geometric queries against a spatial index anyway, so we build the geometry
path regardless and the DOM only saves the point-query case.

**WebGL.** Rejected by ROADMAP §5 and correctly so. It means reimplementing
stroke joins and caps, dashes, corner radii, shadows, gradients, and text
rasterization; it means tessellation and a texture atlas. It buys headroom this
scope does not need. Cost if a future scope demands it: L on its own, and it is
the natural consequence of adding a pen tool.

**Canvas2D — chosen.** One immediate-mode painter over a retained scene graph.
It gives, for free, exactly the paint model in §1: rounded rects, ellipses,
gradients, image drawing with `drawImage`, `shadowBlur`, dashes, clipping for
frames. One renderer means one visual truth, so the preview image, a future PNG
export and the on-screen canvas are the same code path. Performance is
predictable and controllable through mechanisms we own: viewport culling from
the grid index, dirty-rectangle repaint, and a cached bitmap per static frame
subtree.

Structure that makes it testable and keeps the renderer honest:

```
scene (CRDT) → normalize() → layout() → display list → painter (Canvas2D)
                                            ↓
                                     serializer (SVG / PNG / assertions)
```

The **display list** — a flat, ordered array of resolved draw commands
(`{op, matrix, geometry, paint, clip}`) — is the seam. The painter is a dumb
switch over it. Tests assert on the display list instead of on pixels, which is
what makes a renderer testable without a headless-canvas dependency (§14), and
an SVG export later is a second consumer of the same list rather than a second
renderer.

Overlay — selection outlines, transform handles, snap guides, other people's
cursors and labels, comment pins — is a **second canvas in screen space**, over
the scene canvas. Two reasons: handles must be a constant 8px regardless of
zoom, which is trivial in screen space and a division by the zoom factor
everywhere in world space; and a selection change or a moving remote cursor
repaints only the overlay, at zero cost to the scene. Awareness traffic
therefore never triggers a scene repaint, which matters at twenty people in a
room.

Transforms: each node's world matrix is *recomputed* from the root each layout
pass, never mutated incrementally. Incremental matrix mutation accumulates float
error over a long editing session with deep nesting and produces the classic
"my shape drifted by a pixel and I can't undo it".

---

## 7. Text: layout borrowed from the browser, painted on the canvas

The roadmap's instruction is to use DOM text to avoid font shaping entirely. In
a canvas renderer that resolves into a precise split:

- **Shaping** (glyph selection, ligatures, kerning, complex scripts) is done by
  the platform inside `fillText`. We never touch a font file.
- **Paragraph layout** (line breaking, wrapping, bidi paragraph ordering) is not
  something `fillText` does — so it is delegated to the DOM. A hidden measuring
  `<div>` carries the same CSS font shorthand and width constraint;
  `Range.getClientRects()` over its text node yields per-line rects, and a
  binary search over character offsets yields the break offsets. We then draw
  one `fillText` per line at the measured baseline.
- **Editing** is a DOM `contenteditable` overlay, positioned over the text node
  and transformed to match, entered on double-click and torn down on commit. It
  gives IME, spellcheck, native selection, accessibility and mobile keyboards
  for free — every one of which is expensive to fake on a canvas.

The measurement cache is not optional: hug-sized text and auto-layout make
measurement part of the layout pass (§10), and a DOM measure per text node per
frame is a guaranteed 60fps failure. Key: `(text, family, weight, style, size,
lineHeight, letterSpacing, widthConstraint)`; LRU-bounded; invalidated on
`document.fonts` load events.

Fonts are a bundled, self-hosted set (Inter, a serif, a mono) plus the platform
generics; the picker offers nothing else. Render is gated on
`document.fonts.ready` so the first paint is not laid out with fallback metrics.
Residual divergence between browser engines is sub-pixel, affects only hug-sized
boxes, and — because layout is derived and never written to the CRDT (§10) — is
cosmetic and self-healing rather than a data difference between clients.

---

## 8. Hit testing, handles, snapping

**Hit testing** walks the scene front-to-back, transforming the pointer into
each node's local space via the inverse world matrix and running an analytic
test (rect with corner radius, ellipse, line with a stroke-width tolerance, text
box, image box). Frames with `clips-children` short-circuit: a pointer outside
the frame cannot hit anything inside it. Groups are transparent to a click and
selected by their outermost ancestor unless the user is already inside them
(double-click to descend, Escape to ascend) — the standard model, and it is a
state machine, not geometry.

**Marquee, culling and snap candidates** all query one uniform grid over world
AABBs (cell ≈ 256 world units), rebuilt incrementally when a commit changes a
node's bounds. This is the one data structure that has to be right for the perf
story; it is small and pure, so it is unit-tested directly.

**Transform handles** live in the screen-space overlay. Eight resize handles
plus rotate zones just outside the corners; resize runs along the selection's
local axes (so a rotated node resizes sensibly), shift locks aspect, alt resizes
from the centre. Multi-select resize scales the group's bounds and each child's
transform proportionally; font size is only scaled when a single text node is
selected and the modifier is held — silently rescaling type inside a group
resize is the behaviour everyone complains about.

**Snapping** generates candidates from the dragged selection's siblings within
the viewport, plus the enclosing frame's edges and centre, plus an optional
fixed grid. Two rules that make it feel right rather than fighty:

1. The threshold is defined in *screen* pixels (8px) and converted to world
   units by dividing by the zoom — so snapping does not get stickier as you zoom
   out.
2. At most one snap per axis wins, chosen by smallest residual, and the
   alignment guide drawn is exactly the candidate that won. Drawing guides that
   did not affect the position is how users learn to distrust the feature.

Rotated nodes contribute their AABB only. Equal-spacing ("distribute") hints are
an S-sized addition after the basic snap works, not part of it.

---

## 9. The hard part — a mutable tree under concurrent editing

Every other section is fiddly-but-bounded. This one is the section to judge the
plan by, and it has two intertwined halves: **how the scene graph is represented
in the CRDT**, and **when a gesture becomes a CRDT write**.

### 9.1 Representation: flat map, parent pointer, fractional order

The obvious representation — a tree of `Y.Map`s, each holding a `Y.Array` of
children — is the one to avoid. Yjs has no stable move primitive for arrays, so
"move node X from frame A to frame B" becomes delete-from-A + insert-into-B, and
under concurrency that has two well-known failure modes: two people moving X to
different parents produce **two copies of X**, and a move racing a delete
produces **zero copies**. Duplicating or losing a user's object is not a merge
artefact you can explain away in a design tool.

The representation instead:

```ts
doc.getMap('nodes'): Y.Map<nodeId, Y.Map>   // FLAT. every node, no nesting.
  parent:    string      // node id, or 'root'
  order:     string      // fractional index among siblings
  transform: Y.Map       // {x, y, w, h, rot} — one key, see below
  props:     Y.Map       // fill, stroke, radius, opacity, ...
  text:      Y.Text      // text nodes only
doc.getMap('meta')                          // canvas background, doc-level settings
```

What each choice buys:

- **`parent` is a plain LWW key.** Two concurrent moves of the same node resolve
  by Yjs's deterministic last-writer rule: one parent wins, on every client,
  with no duplication and no loss. The tree is *derived* by grouping on `parent`
  rather than stored as containment.
- **`order` is a fractional index string** (base-62, midpoint between
  neighbours, plus a short random tail). Sibling order — which is z-order —
  survives concurrent insertion without a list CRDT and without renumbering.
  Two clients inserting at the same slot get different keys because of the tail;
  identical keys tie-break on node id. Keys grow ~1 char per repeated
  same-slot insertion; at 64 chars that node's sibling run is renormalized
  locally. Global renormalization is forbidden: it rewrites every sibling and
  turns a keystroke into a document-sized update that conflicts with everything.
- **`transform` is one key, not five.** Per-key LWW on x/y/w/h means two people
  dragging and resizing the same rectangle can produce a position from one and a
  size from the other — a geometry nobody asked for. Grouping them makes a
  single node's geometry atomic per gesture; the loser's whole drag is
  discarded, which is both explainable and undoable.
- **Text content is `Y.Text`.** The one place a real sequence CRDT is needed,
  and it comes free.

### 9.2 The three anomalies, and how they are resolved

A flat map with parent pointers removes duplication and loss, and leaves three
states that a naive materializer would render as a broken document. All three
are handled in one place: **`normalize(state) → tree`, a pure function every
client runs on every materialization.** Because it is a pure function of CRDT
state, every client computes the same repaired tree with no extra messages, no
coordination and no server involvement — which is the property that makes this
approach viable at all.

1. **Cycles.** A moves X into Y while B moves Y into X. Both writes are valid
   LWW winners; the result is a detached loop that belongs to no root. Kleppmann's
   move-operation algorithm fixes this properly by undoing and replaying
   operations in a total order — which Yjs's structure does not give us.
   `normalize` instead detects cycles by walking parents with a visited set and
   breaks each one deterministically: within a cycle, the node with the
   lexicographically smallest id is reparented to `root`. Deterministic rule,
   identical output on every client, no message exchanged. The user sees an
   object pop out to the canvas top level — visible, undoable, and never lost.
2. **Orphans.** A deletes frame F (as a subtree, in one transaction, because
   that is what the user meant) while B moves shape S into F. S survives with a
   dangling parent. `normalize` reparents it to `root`. Same rule, same outcome:
   the shape you moved into a frame someone was deleting comes back at the top
   level.
3. **Concurrent grouping.** A groups {1,2,3} while B groups {3,4,5}. Two group
   nodes are created; node 3's parent is decided by LWW; the other group holds
   two children. Nothing is lost, nothing duplicated, and the result is
   comprehensible. Accepted as-is — the alternative is a lock, and a lock on a
   multiplayer canvas is a worse product than an occasionally surprising group.

The invariant this buys, and the one the tests in §14 assert: **no sequence of
concurrent operations loses a node, duplicates a node, or produces a structure
that is not a tree.** The repair rule is always "reparent to root", which is
visible and undoable, never "delete".

Optional refinement, deliberately not in the critical path: the client that
performs the *next* mutation after observing a repair may write the repair back
so the state stops being derived. It is an optimization only — correctness comes
entirely from every client computing the same repair.

`normalize` is also where malformed state from a hostile or buggy client dies:
unknown node types, missing transforms, a `parent` naming a non-existent node,
depth beyond 32. It fails closed to `root` and never throws — a canvas that
white-screens on one bad node is worse than a canvas with one node in the wrong
place.

### 9.3 Commit granularity: a gesture is one update, not sixty

The second half. A drag at 60fps that writes to the CRDT every pointermove
produces ~60 updates per second per user, each an `INSERT` into `collab_updates`
holding the document row lock (015's `head_seq` counter serializes appends per
document), plus a fan-out through the hub and NATS to every room member. Twenty
people nudging things is thousands of rows and megabytes of log per minute, and
compaction never catches up.

So: **local drag state layered over the CRDT scene, committed once on
pointerup.** During the gesture the renderer composites `{node → provisional
transform}` on top of the materialized scene; the CRDT is untouched. Other
people see the motion live because the in-flight transform rides **awareness** —
ephemeral, never persisted (015 is explicit that awareness must never reach
Postgres), and already rate-limited on its own budget at
`internal/ws/client.go:60-61`. Awareness for a 500-node multi-select drag sends
the selection ids once and a delta per frame, not 500 transforms.

Consequences, stated so nobody rediscovers them in review:

- The update log is proportional to *gestures*, not to frames. A minute of
  frantic editing is tens of updates, not thousands.
- Conflict granularity becomes the gesture, which is exactly the unit a user
  can reason about and undo.
- A crash mid-drag loses that drag and nothing else.
- Bulk mutations must be batched: the WebSocket path caps a collab payload at
  32 KiB (`internal/ws/client.go:39`) and the HTTP append path at 1 MiB
  (`015_collab.up.sql:86`). Pasting 2,000 nodes is therefore several Yjs
  transactions of ~200 nodes each over the HTTP endpoint — not one giant update,
  which would be rejected at the constraint and is not chunkable after the fact.
- **Images are references, never bytes.** A data-URI image inside the CRDT would
  blow through both caps and then through the 16 MiB snapshot ceiling
  (`015_collab.up.sql:99`). The client asserts this on paste and on import.

Undo is Yjs's `UndoManager` scoped to the local client's origin, so undo never
reverts someone else's work. It composes with §9.2 because a repair is not an
operation — undoing a move that was repaired re-derives the tree from the
restored state.

---

## 10. Layout: derived, never materialized

A frame may carry `layout: {direction, gap, padding, align, sizing}`. Its
children are positioned by the layout pass **at render time**; their stored
`transform.x/y` is ignored while their parent has a layout. Dragging a child
inside a layout frame changes its `order`, not its coordinates.

Derived rather than materialized, and this is a real decision:

- Materializing means every client that opens the document writes computed
  positions back — write amplification proportional to readers, and two clients
  with slightly different font metrics writing *different* positions in a loop.
- Derived means layout divergence between browsers is a cosmetic sub-pixel
  difference in a pure function, not a data conflict. Nothing to reconcile
  because nothing was written.

Sizing modes are fixed / hug / fill. **Hug makes text measurement part of
layout**, which is why §7's measurement cache is load-bearing rather than an
optimization: a layout pass on a frame containing 50 hug-sized text nodes must
not do 50 DOM measurements. Nested layout frames resolve bottom-up in one pass
over the (already computed) tree order.

Cut: wrap, absolute-positioned children escaping a layout frame, min/max
constraints. Each is S–M individually and none is needed to make row/column
useful.

---

## 11. Components

A main component is an ordinary subtree flagged `isComponent`. An `instance`
node holds `componentId`, its own transform, and an `overrides` map keyed by the
**node id inside the component** — not by a child-index path, so an override
survives the component's children being reordered. Overrides are limited to text
content, fill colour and visibility.

Instances are resolved at materialization: the component subtree is walked, the
overrides applied, and the result appended to the display list with the
instance's transform. Nothing is copied into the CRDT, so editing the main
updates every instance for free, and an instance costs one node.

Concurrency and lifecycle, decided rather than discovered:

- Editing the main while someone overrides an instance: no conflict, they are
  different keys in different subtrees; overrides win at resolution.
- Deleting a main that has instances: blocked in the UI, and if it happens
  concurrently the instances render as a labelled empty placeholder. There is no
  "bake the last known state" fallback — that means storing a copy of the
  component in every instance, which is the design-system feature set (§16) in
  disguise.
- Nested components (instance inside a main) are cut: they require an override
  path model and a resolution order, and they are the doorway to component
  libraries.

---

## 12. Derived outputs and the seams

**Preview image.** The server cannot render the document, so the client does.
Reuse `Hub.SendToRoomLeader` (`internal/ws/room.go:179`) — the mechanism the
collaboration layer already has for asking one client in a room to produce a
snapshot — with a `design.preview` request emitted on the same idle trigger as
compaction. The leader renders the used bounds into an offscreen canvas at
≤2048px and `PUT`s it. `source_seq` resolves the "up to three replicas each pick
a leader" duplication that room.go:179 already documents.

*Production detail that will otherwise be found the hard way:* drawing a
cross-origin image into a canvas taints it and `toBlob` then throws. In any
deployment where the SPA and API are on different origins, assets must be
fetched as blobs with the `Authorization` header (CORS is already configured at
`internal/app/app.go:391`) and turned into bitmaps via `createImageBitmap`,
never loaded as `<img src=fileURL(id)>`. The `?token=` query form used by
`app/src/api/files.ts` is fine for display and fatal for preview generation.

**Search.** The same leader trip produces the text digest: text-layer content
plus node names, deduplicated, capped at 64 KiB. `PUT .../digest` publishes an
event; the worker's existing indexer writes `search.Doc{Type: TypeDesign, Title:
<file name>, Content: <digest>, ACL: <object keys>}`. `TypeDesign` is already
declared (`internal/search/doc.go:28`) and the doc constructor mirrors
`FileDoc.Doc()` at `:323`. The trust boundary: the digest is client-supplied
text and is treated as such (bounded, stored as text, never HTML), while the ACL
is derived server-side from the object — a client cannot make its design
findable by someone who cannot read it.

**Comments.** Reuse the comment surface Drive/work-tracking builds. A canvas
comment anchors to `(nodeId, local offset)` rather than to a viewport
coordinate, so it follows the object when it moves and survives a layout change.
Pins draw on the overlay canvas. If the anchor node is gone, the comment falls
back to the thread list — never silently disappears.

**Mobile.** `index.native.tsx` renders the preview image, the comment thread and
the file metadata, with an explicit "editing is available on the web" affordance.
The preview may be stale by one editing session; that is the honest cost of
ROADMAP §3 option 1 and should be labelled with a timestamp rather than hidden.

---

## 13. Sequencing

Ships in this order; the numbering is dependency, not priority.

1. **Skeleton (S–M).** Registry handler for
   `application/vnd.superops.design`, a design file opens, an empty canvas binds
   to a collab document with `resource_type='design'`, presence works. Proves the
   substrate end-to-end before any of the expensive work starts. If this is hard,
   the collaboration layer is not done and everything after it is at risk.
2. **Scene model + `normalize` + ops (M).** Pure, headless, fully unit-tested
   before a pixel is drawn (§14). This is the piece to get right first because
   everything downstream reads its output.
3. **Renderer + basic interaction (L, long pole).** Display list, painter,
   pan/zoom, culling, hit testing, select/move, the overlay. The first version
   anyone can use.
4. **Handles, marquee, z-order, grouping, snapping (M).**
5. **Text (M–L).** Measurement, cache, canvas painting, contenteditable overlay.
   *Parallelizable with 3–4* — it is a self-contained subsystem behind the node
   interface and a different person can own it from step 2 onward.
6. **Auto-layout (M).** Needs 5's measurement for hug sizing.
7. **Components (M).**
8. **Panels (M).** Layers tree, properties inspector, assets. Unglamorous, real,
   and easy to under-budget.
9. **Derived outputs + seams (S–M).** Preview, digest, search, comment pins.
   Backend routes are S and can land any time after step 1 — a second person can
   do the whole backend slice in week one.
10. **Perf pass (M).** 5k-node fixture, dirty rects, frame-cache, measured
    budgets.

Parallel tracks once step 2 lands: **renderer/interaction** (the long pole),
**text + layout**, **panels + backend + seams**. Three tracks, one shared model.

---

## 14. Verification

**Model tests — the highest-value tests in the phase, and cheap because
`normalize` is pure.** In `vitest`, which the client already runs
(`app/package.json`), with no infrastructure at all:

- Property test: generate random op sequences (create / move / reorder / delete
  / group / set-property) against two `Y.Doc`s, apply the updates to each in
  different (shuffled) orders, materialize both, assert **structural equality**.
- Invariants after every materialization: the output is a tree; no node appears
  twice; every node created and not deleted appears exactly once; depth ≤ 32.
- The three anomalies from §9.2 as named, hand-written cases: mutual move
  (cycle), move-into-deleted-frame (orphan), overlapping concurrent group. Each
  asserts the specific repair, not just "does not crash".
- Fractional index: concurrent same-slot insertion, key-length growth bound,
  the local renormalization path.

**Renderer tests** assert on the display list, not on pixels — the exact reason
§6 puts a serializable display list between the scene and the painter. No
headless-canvas dependency, and a diff that is readable in CI output. Snapping,
hit testing and the grid index are pure functions tested directly.

**Go integration tests**, added to `backend/test/integration` (real
Postgres/Redis/NATS/MinIO/Meilisearch, `harness_test.go`):

- `TestDesignAssetCrossTenant` — a member of another workspace can neither
  `POST` an asset to nor `GET` an asset of a design they cannot read. Sits
  beside the existing `TestCrossTenantSearch` / `TestCrossChannelIDOR` in
  `tenancy_test.go`.
- `TestDesignAssetSurvivesObjectGC` — **the one that catches §2's gap.** Upload
  an asset, backdate its `created_at` past `objectGCGrace`, run the GC job body,
  assert the row and the object are still there. Without this test the failure
  is a design whose images vanish a day after it was made, discovered by a user.
- `TestDesignPreviewRequiresWrite` — a read-only collaborator gets 403 on
  `PUT /preview` and `PUT /digest`.
- `TestDesignPreviewSeqOrdering` — an upload carrying a lower `source_seq` than
  the stored one is discarded, not applied.
- `TestDesignDigestIndexed` — digest → event → worker indexer → the design
  appears in unified search for a member and does not appear for a non-member.
- `TestDesignRoomRevocation` — extend the room/realtime coverage: a collaborator
  whose capability is revoked mid-session stops receiving room frames
  (`RevokeRoom`, `internal/ws/room.go:226`).

**Performance harness (local, not CI).** A generated fixture at 500 / 2,000 /
5,000 nodes with a realistic mix of frames, text and images. Recorded budgets:
pan/zoom frame ≤ 8ms, single-node drag ≤ 8ms, 200-node multi-drag ≤ 16ms, cold
open of a 5k-node document ≤ 2s. Numbers in the repo so a regression is a diff.

**Two-browser checklist** for the cases automation will not cover honestly:
concurrent move of the same node, move-into-frame-being-deleted, simultaneous
group, one client offline for 60s then reconnecting mid-edit, revoked mid-drag.

---

## 15. Risks and failure modes

**Update-log growth.** A design's Yjs state is far larger than a text document's.
Mitigated by §9.3's gesture-granular commits, images-by-reference, and 015's
existing compaction; the residual risk is a document that grows past the 16 MiB
snapshot ceiling. Add a client-side node-count warning at 5,000 and a hard
document limit at 10,000 nodes — a limit that exists and is explained beats one
discovered at a constraint violation.

**A single hot document.** 015 serializes appends per document behind the
`head_seq` row lock. Twenty people gesture-committing is fine (tens of writes
per second); twenty people with a buggy client committing per frame is not. The
per-connection collab budget (`internal/ws/client.go:60`, 40/s) is the backstop
and should be treated as an alarm threshold, not a ceiling to design against.

**Awareness fan-out.** Twenty people × 30 cursor updates/s × 20 recipients is
12,000 frames/s through the hub and NATS for one document. Throttle awareness to
15 Hz, coalesce to at most one frame per animation frame, and soft-warn above 20
people in a room. Cheap to do now, invisible until a company-wide review meeting
opens the same file.

**Assets deleted by the orphan collector.** §2. It is the highest-severity item
in this plan because it is silent, delayed and unrecoverable, and it is already
true of the code as it stands.

**Preview generated from a stale or hostile client.** Anyone who can write can
already draw anything on the canvas, so the preview's trust boundary is
unchanged — but the size caps, the server-side content sniff and the `write`
requirement all still apply, and a read-only viewer must never be able to change
what Drive shows.

**Memory in a long session.** Undo history, the Yjs document, cached frame
bitmaps and decoded images on a 5k-node file. Bound the undo stack, evict frame
caches by LRU, and downscale decoded images to a display variant rather than
holding 4000×3000 bitmaps.

**Font availability skew.** Bundled fonts plus render gated on
`document.fonts.ready`. Because layout is derived (§10), skew is cosmetic and
never a data conflict — the strongest single argument for that decision.

**The RN fork biting later.** If mobile editing is requested, none of this
transfers: it is a WebView host (ROADMAP §3 option 2) plus a bridge, or a
second renderer. The `.web/.native` split keeps that decision cheap to *make*,
not cheap to *implement*.

---

## 16. Scope guards, priced

These are ROADMAP §7 re-planning triggers. Prices so a future "can we just add…"
is answered with a number.

| Addition | What it actually costs |
|---|---|
| **Pen tool** | A path node type (segments, bezier handles, open/closed), a path-editing interaction mode, hit testing on curves (flatten + distance), and stroke geometry on arbitrary paths. **L**, and it makes the object count and overdraw arguments in §6 shakier — the WebGL question reopens. |
| **Boolean path operations** | Robust path intersection: self-intersection, winding rules, numerical degeneracy. Not hand-rollable; a third-party geometry library becomes a core dependency and a correctness surface. **L–XL on top of the pen tool**, which it requires. |
| **Plugin runtime** | Sandbox (worker or iframe), a stable scene API, a permission model, versioning, and a permanent backwards-compatibility commitment. Same class of problem as ROADMAP §5's workflow sandbox. **L–XL**, plus ongoing cost forever. |
| **Prototyping engine** | A second document model (flows, hotspots, transitions), a player mode, and a second interaction layer that duplicates hit testing in preview semantics. **L**. |
| **Design-system tooling** (variables, styles, shared libraries) | The sharpest one: cross-file component references break "one CRDT document = one object". You get cross-document references, permission composition (rendering an instance of a component in a file you cannot read), version pinning and update propagation. It turns a client-side project into a distributed-systems project. **L–XL**, and it invalidates §9's representation. |
| Multi-page files | **S**. The one genuinely cheap item on this list. |
| Nested components | **M**: an override path model and a resolution order. |
| Server-side rendering/export | Headless Chromium in `cmd/worker`: a heavy new dependency, a new resource profile, and a second code path that must agree with the client's renderer pixel-for-pixel. **M–L**, and it is the reason §6 keeps one renderer. |

---

## 17. Sizing

| Piece | Size |
|---|---|
| Backend: 4 routes, migration 022, GC invariant, search seam | **S** |
| Editor shell, registry handler, collab binding | **S–M** |
| Scene model, `normalize`, ops, undo, clipboard | **M** (hard, not large) |
| Renderer: display list, painter, culling, dirty rects, overlay | **L** |
| Interaction: select/drag/resize/rotate/marquee/pan/zoom/keyboard | **L** |
| Text: measurement, cache, painting, editing overlay | **M–L** |
| Auto-layout | **M** |
| Components + overrides | **M** |
| Snapping + guides | **M** |
| Panels: layers, properties, assets | **M** |
| Comment pins | **S–M** |
| Native read-only surface | **S** |
| Perf pass + fixtures | **M** |

**Total: XL**, consistent with ROADMAP §5's L–XL re-pricing, and ~90% of it is
client code — which is the evidence that the substrate (§3b, the collaboration
layer, plan 00) did its job.

**Long pole: renderer + interaction**, which one person owns end-to-end because
splitting them across two people produces two interaction models. Text is the
second track and starts as soon as the scene model exists. The model work (§9)
is the *risk* long pole even though it is not the size long pole: it is small,
pure, testable, and everything else is wrong if it is wrong. Do it first.

---

## 18. Open questions for a human

1. **Export.** Is client-side PNG/SVG export in v1, or is the Drive preview
   enough? It is S–M if the display list exists, and it changes nothing
   architecturally — but it is not currently scoped.
2. **Drive asset parenting (§2).** Does Drive ship "a file owned by an arbitrary
   object", or does this phase carry `design_assets` as a fallback? This is the
   only backend dependency that can block the phase.
3. **Confirm ROADMAP §3 option 1 for this pillar specifically** — web edits,
   native shows a preview. Everything in §5, §6 and §12 assumes it, and it is
   the one assumption that would invalidate the renderer choice.
4. **Fonts.** Which bundled set? And is customer font upload ever expected — it
   is a re-planning trigger, not a setting.
5. **Room size.** Expected concurrent editors per file. It sets the awareness
   budget and the soft cap in §15, and 5 vs 50 is a different plan.
6. **Multi-page files.** Cut here at S. Confirm nobody is assuming them.
