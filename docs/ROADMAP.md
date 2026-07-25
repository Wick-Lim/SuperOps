# SuperOps — all-in-one roadmap

Target: one self-hosted product covering what a company needs — chat, huddles,
drive, documents, spreadsheets, design, email, work tracking and workflow
automation.

This is written against the code as it stands (94 routes, 12 migrations, the
packages under `backend/internal/`), not against a blank page. Sizes are
relative (S/M/L/XL), not calendar estimates — absolute time depends on headcount,
which this document does not assume.

---

## 1. The thesis, and where all-in-one actually fails

Five separate apps in one repository lose to five best-of-breed tools. Every
time. The pillars are not what makes this worth using — the **seams** are:

- a document linked in a channel that renders inline, with its permissions
- a file in Drive attached to a message without a copy
- a workflow triggered by "message posted in #alerts" that files a doc
- one search that returns a message, a file and a doc in the same list
- one inbox that tells you what needs your attention across all nine
- one identity, one permission model, one audit trail

None of those work if each pillar invents its own object model. So the ordering
below front-loads the **shared spine** and treats the pillars as things that
plug into it. Building Drive before the spine means building it twice.

**Corollary:** every pillar shipped shallow raises the cost of the next one,
because the seams multiply. Nine pillars is thirty-six possible pairs.

### The pillars are not the same kind of thing

Sorting them by *kind* matters more than sorting them by priority, because it
determines whether "build it" is even the right verb.

| Tier | Pillars | What they are |
|---|---|---|
| **A — extensions of what exists** | Drive, Huddle, Work tracking | Mostly CRUD, state and views over the existing identity/permission/search/realtime spine. Hard parts known and bounded. |
| **B — new subsystems** | Email, Workflow | Each brings one hard problem that is not CRUD: deliverability, safe execution. |
| **C — collaborative editors** | Docs, Design surface, Spreadsheet | Three surfaces over one object model. Each has its own hard core — block model, canvas, formula engine — but they share storage, permissions, comments, presence and **real-time collaboration**. |

Tier A is where the leverage is. Tier B is where the on-call burden is. **Tier C
is where the schedule is**, and it is the reason §3b and the collaboration
layer in Phase 0 matter more than any individual pillar: three editors built on
a shared substrate is a project, three editors built separately is three
projects that never quite agree with each other.

Nine pillars, three of them L–XL editors. Stated plainly so the sequencing in
§6 is read as a sequence rather than a wish list — the order exists because
capacity is finite, not because any item is unimportant.

---

## 2. Where we are

Genuinely solid and reusable:

| Asset | State |
|---|---|
| `internal/authz` | 15 methods, single point of every membership/role decision. **The thing to generalize.** |
| Files + MinIO | `files` table, upload/download, workspace-scoped authz, orphan GC in the worker |
| Meilisearch | one `messages` index, filtered to the caller's readable channels |
| NATS + JetStream | durable consumers with ack/retry/terminal paths; 5 consumers, 4 job loops |
| WebSocket hub | multi-replica, NATS-relayed, per-connection seq, revocation |
| Postgres | 12 migrations, keyset pagination, real constraints |
| Deploy | Compose + Helm, both verified running |

Known gaps that become blocking as scope grows:

- **No SSO/SCIM.** Fine for a chat app, mandatory when it is the whole stack.
- **No mail sending at all.** Invitations are copy-pasted by hand today.
- **Permissions are workspace/channel-shaped.** Docs, files and workflows need
  object-level ACLs. This is the single most important thing to get right.
- **Search indexes messages only.**
- Notifications are per-message; there is no cross-object "inbox".
- Audit covers a handful of admin actions.

---

## 3. The architectural fork you have to decide first

Four pillars — **Docs**, **Spreadsheet**, **Design surface** and **Huddle** —
collide with the same bet: one React Native codebase for web + iOS + Android.

- None of the three editors has a good native RN implementation. The mature
  block editors (TipTap, Lexical, ProseMirror) are DOM-based, a design canvas
  wants SVG/Canvas2D, and a virtualized grid wants DOM.
- WebRTC needs `react-native-webrtc`, which **does not run in Expo Go** — it
  requires a development build and a config plugin.

Three ways out, pick one before starting either pillar:

1. **Web-first for those surfaces.** Docs edit on web, mobile reads and
   comments. Huddle joins on web, mobile gets audio-only or a link-out.
   *Cheapest, and honest about where the work happens.*
2. **WebView-hosted editor + Expo dev build.** Keeps one app, adds a build
   pipeline and a JS bridge. *Medium cost, medium compromise.*
3. **Split the native app out.** Full control, doubles the client surface.
   *Only if mobile is a primary workflow, not a companion.*

**Recommendation: (1) now, revisit at the point mobile editing is actually
requested by users.** It is reversible; the other two are not cheaply.

---

## 3b. Drive is the substrate; editors are handlers

The right shape for this product, and the one the rest of this document now
assumes:

**Drive owns the object. Editors register against a file type.** Opening a file
dispatches to whichever surface handles it — a document opens the block editor,
a design file opens the design surface, a spreadsheet would open a grid.

```
              ┌──────────────── Drive ────────────────┐
              │ file id · type · versions              │
              │ ACL · sharing · comments · activity    │
              │ search index                           │
              └───────────────────┬───────────────────┘
                                  │
              ┌───────── collaboration layer ─────────┐
              │ CRDT document · awareness · presence   │
              │ (one implementation, all editors)      │
              └───────────────────┬───────────────────┘
                                  │  dispatch on file type
        ┌──────────┬──────────────┼──────────────┬──────────┐
        ▼          ▼              ▼              ▼          ▼
   block editor  design       spreadsheet     preview    (future)
     (docs)      surface      (grid+formula)  (built-in)
```

Why this matters more than it looks:

- **Storage, ACLs, sharing links, versioning, comments, search and activity are
  written once** and every editor inherits them. Without this, each editor
  reinvents six subsystems — which is precisely the "five apps in one repo"
  failure from §1.
- It makes **Drive-first** (Phase 1) not just cheapest but structurally
  required. Everything else plugs into it.
- It turns **Docs from a pillar into a handler**. The editor is still real work;
  the surrounding 60% is not, because Drive already did it.
- It keeps the editors **independently replaceable**. Each occupies one slot
  behind one contract, so a surface can be rewritten — or temporarily stood in
  for by something third-party — without touching storage, permissions or
  sharing.

What it does **not** change: the cost of what runs *inside* an editor pane. The
registry is a day of work. Each of the three editors is months (§5). Where a
file lives has no bearing on how hard it is to render and edit its contents.

---

## 3c. Deployment-dependent capabilities are a category, not one-offs

Mail sending exposed a pattern, and TURN is the second instance of it: some
capabilities have **no single correct implementation, because the right answer
is a property of the deployment rather than of the product.**

- **Mail.** Almost every cloud provider blocks outbound port 25 (AWS, GCP,
  Azure, DigitalOcean, most VPS), so a cloud deployment physically cannot
  deliver mail itself and must relay. On-premises it can, and avoids routing
  company mail through a third party.
- **TURN.** Roughly 15–20% of WebRTC connections cannot go peer-to-peer —
  symmetric NAT, restrictive corporate firewalls — and simply fail without a
  relay. Running one needs a public IP, UDP/TCP 3478, TLS 5349, a wide relay
  port range and real bandwidth, since all relayed media flows through it. That
  is reasonable on-prem and often unwanted in a cloud deployment.

The same shape already applies to storage (MinIO vs S3 vs GCS), search
(Meilisearch vs Elastic vs Postgres FTS), push (Expo vs direct APNs/FCM), and
will apply to SSO providers and the workflow execution backend.

**Principle: one interface per capability, transport chosen by config,
validated at startup, with a safe default.**

```go
type Sender interface { Send(ctx, *Message) error; Name() string }   // mail  — done
type Sender interface { Send(ctx, []Push) error; Name() string }     // push  — done
type ICEProvider interface { Servers(ctx, room) ([]ICEServer, error) } // TURN — to build
type Storage   interface { ... }   // files  — currently MinIO-shaped, works via S3 API
type Index     interface { ... }   // search — currently Meilisearch-shaped
```

Three rules that make this worth having rather than ceremony:

1. **The default must be safe, not convenient.** Mail defaults to `log` so a
   fresh deployment cannot mail real people by accident. Push defaults to off.
   TURN should default to STUN-only with a startup warning, not to a
   half-configured relay that fails at call time.
2. **Misconfiguration must fail at boot, not at first use.** Selecting a
   transport without its credentials is a startup error. Silent
   misconfiguration in this category is invisible until a user is affected —
   an invitation nobody receives, a call that connects for everyone except the
   person behind a corporate firewall.
3. **Give the operator a way to verify.** An admin-triggered test that sends a
   real message, or fetches real ICE candidates, and reports the actual error.

### TURN specifically

Options, in the order worth considering:

| Approach | When |
|---|---|
| **Comes with the SFU** | If LiveKit is chosen for media (§5 recommends it), self-hosted LiveKit **embeds a TURN server** — the question largely collapses into the SFU decision. It still needs the public IP, ports and TLS certificate; bundled is not free. |
| **Bundled coturn** | Compose service + Helm subchart, for on-prem where the network is yours. Ships working out of the box. |
| **External provider** | Twilio NTS, Cloudflare, Metered, Xirsys. Right for cloud deployments that do not want to run or scale a relay. |
| **STUN only** | Works on permissive networks, fails for ~15–20% of users. Acceptable as a *documented* default, never as a silent one. |

Whichever is used, **credentials must be short-lived and minted per session**
by the backend (`GET /huddles/{id}/ice`, HMAC time-limited, RFC 7635-style).
Shipping static TURN credentials to clients hands anyone who opens devtools a
free media relay.

---

## 4. Phase 0 — the spine (before any new pillar)

Nothing here is glamorous and all of it is load-bearing.

| Work | Size | Why now |
|---|---|---|
| **Object-level permission model** | L | Generalize `internal/authz` from (workspace, channel) to (subject, object, capability) with inheritance. Every pillar needs it and retrofitting eight is worse than designing one. |
| **Collaboration layer (CRDT)** | L | Three editors need multiplayer. Building it once is the difference between one project and three. See below. |
| **Mail sending** | S | Invites are copy-pasted by hand *today* and password reset does not exist. Needed regardless of which email scope comes later. **Transport is operator-chosen** — see below. |
| **Unified search** | M | Generalize the Meili index from `MessageDoc` to typed objects with an ACL field, so one query spans messages, files and docs. |
| **Unified inbox / notifications** | M | One "needs attention" surface. Today's notification table is close; it needs an object reference instead of a message id. |
| **SSO (OIDC) + SCIM** | M | Becomes a procurement blocker the moment this is the company's whole stack. |
| **Audit coverage** | S | Cheap now, expensive to backfill. |

### The collaboration layer, concretely

Docs, the design surface and the spreadsheet all need the same thing: several
people editing one object at once, with cursors and selections visible. Built
per-pillar that is three implementations, three sets of merge bugs and three
different answers to "what happens when the network drops". Built once it is
infrastructure.

Shape that fits what already exists:

- **CRDT in the client, opaque to the server.** Yjs is the mature choice. The
  backend stores and fans out update blobs without understanding them — no Go
  CRDT implementation, no server-side merge logic. This is the pragmatic fork
  and it should be taken deliberately.
- **Transport is the existing WS hub.** It is already multi-replica via NATS
  with per-connection sequencing and revocation. A collaboration room is a
  subscription like any other, and `internal/authz` already answers who may
  join — generalized to objects by the permission work above.
- **Persistence is an append-only update log in Postgres plus periodic
  snapshots to MinIO.** Same pattern the retention and object-GC jobs in the
  worker already follow.
- **Awareness** (cursor, selection, who is here) rides the same channel and is
  ephemeral — it never touches Postgres.

The document *models* stay per-editor: a block tree, a scene graph and a cell
map are different shapes. What is shared is transport, persistence, awareness,
access control and reconnection.

---

## 5. The pillars

### Drive — *start here*

**Exists:** MinIO, `files` table, upload/download, authz, orphan GC.
**Missing:** folder tree, sharing links, versioning, trash, quotas, previews.
**Hard part:** nothing genuinely hard. Desktop sync (à la Dropbox) is hard —
treat it as out of scope for v1.
**Size:** M. Highest leverage: roughly 60% of it exists, and it is what Docs
and Email attach to.

### Docs — *the daily-use pillar*

**Exists:** authz, Meilisearch, MinIO for embeds, workspace model.
**Missing:** block editor, document tree, comments, per-doc permissions.
**Hard part:** (a) the RN fork above; (b) real-time collaborative editing.
**Size:** L. Multiplayer is no longer priced here — it comes from the shared
collaboration layer in Phase 0.
**Cut for v1:** database views, formulas in tables, page-level permissions
beyond inherited Drive ACLs, public publishing.

*(An earlier draft cut CRDT from Docs v1 in favour of single-writer locking.
That is withdrawn: with the design surface and spreadsheet also needing
multiplayer, cutting it here saves nothing and leaves two editing models to
reconcile later.)*

### Huddle — *cheapest differentiation*

**Exists:** the WebSocket hub is already a correct signaling channel; presence
is already there; authz answers who may join.
**Missing:** media path, TURN, client UI.
**Hard part:** media, not signaling. Mesh P2P works to ~4–6 people; past that
you need an SFU. **Use LiveKit** (Go, self-hostable, Apache-2.0) rather than
building one — it fits the stack, removes most of the risk, and **embeds a TURN
server**, which folds most of §3c's TURN question into this one decision.
Budget for the relay regardless: ~15–20% of connections cannot go
peer-to-peer, and relayed media is real bandwidth on a real public IP.
**Size:** M with LiveKit, XL without.
**Cut for v1:** recording, transcription, >10 participants.

### Email — *highest operational cost*

**Exists:** worker/JetStream is the right shape for inbound processing, MinIO
for attachments, Meilisearch for search.
**Missing:** everything else.
**Hard part:** **deliverability is not a coding problem.** SPF/DKIM/DMARC, IP
and domain reputation, warmup, bounce and complaint handling. A fresh
self-hosted sender lands in spam. Inbound means spam filtering, which is its
own discipline.
**Size:** shared inbox = L. Personal mailboxes = XL.
**Recommendation:** shared inbox (`support@`) only. It fits the existing model
(a mailbox is a channel with an address), and it is the version where
self-hosting is a feature rather than a liability. **Do not build personal
mailboxes** — see cuts.

### Work tracking — *the best-fitting addition*

**Exists:** authz, workspaces, notifications, search, attachments, and the
comment surface Docs will also need.
**Missing:** issue model, state machine, board/list/timeline views, saved
queries, estimates, cycles.
**Hard part:** none technically — it is CRUD, a state machine and query views.
The hard part is **product discipline**. Jira's cost is its configurability:
custom fields, permission schemes, per-project workflows. Linear won by
refusing almost all of it.
**Size:** M–L.
**Recommendation:** build it, scoped like Linear rather than Jira. It also
produces the best seams of any pillar — issue from a message, issue linked in a
doc, workflow triggered on a state change, huddle from an issue.
**Cut for v1:** custom fields, configurable workflows per project, time
tracking, sprint burndown.

### Design surface — *scope decides everything here*

Two very different products share the word "design tool", and they are an order
of magnitude apart.

**Figma/Penpot-class — do not build.** Not an application over a database:
a GPU scene graph, vector boolean operations and path math, font loading and
shaping, a constraint solver, plugins, export pipelines, colour management.
Figma spent roughly four years pre-beta with specialists and pioneered browser
WebGL rendering to get there. None of the existing spine reduces any of it.
This alone would cost more than the other seven pillars combined.

**A scoped design surface — buildable, and the current target.** Frames,
shapes, text, images, reusable components, simple row/column layout, pan/zoom,
snapping and multiplayer. No pen tool, no boolean path ops, no plugin runtime,
no prototyping engine, no design-system tooling.

*(Assumption to confirm: that is what "pencil.dev level" means here. If it
includes something specific — a pen tool, variables, prototyping — say so,
because those are exactly the line items that move this back toward the first
category.)*

What the reduced scope actually removes, and why the estimate changes:

| Dropped | Why it was the expensive part |
|---|---|
| Pen tool, boolean ops | Path math and geometry are a large share of a vector editor |
| Font shaping | Using DOM/SVG text delegates layout to the browser entirely |
| GPU renderer | At a few thousand objects, Canvas2D or SVG is sufficient |
| Constraint solver | Absolute positioning in v1; flex-like row/column is tractable |
| Plugins, export pipelines | Pure surface area, no architectural depth |

What remains hard: canvas interaction (pan/zoom, hit testing, selection,
transform handles, snapping) — fiddly but bounded — and **real-time
collaboration**, which is the part explicitly wanted and is the genuinely hard
remainder.

**Size: L–XL.** Comparable to the Docs block editor, not to Figma. My earlier
"years" figure was priced against Figma-class scope and does not apply here;
correcting it.

**Decision: build it, do not embed Penpot.** The concern about Figma-class cost
was raised and answered by scoping down; at this scope building is defensible,
and an embedded third-party surface would never share the object model,
comments, permissions or presence that make the seams (§1) worth anything.

The scope guards above are what keep that true. Adding a pen tool, boolean ops,
a plugin runtime or a prototyping engine moves this back into the first
category — treat any of them as a re-planning trigger, not a backlog item.

### Spreadsheet — *the third editor*

**Exists after Phase 0:** Drive object, ACLs, comments, collaboration layer,
search. The registry (§3b) makes it a handler, not a pillar.
**Missing:** virtualized grid, cell model and formatting, **formula engine**.
**Hard part:** the formula engine — parser, evaluator, a dependency graph with
topological recalculation, dirty propagation and circular-reference detection.
Everything else is comparatively routine: grid virtualization is well-trodden,
and cells are the *easiest* of the three document models to make collaborative
because they are independent values rather than a shared tree.
**Size:** L, concentrated almost entirely in the formula engine.
**Scope guard:** ~50–80 functions covers real use; Excel's ~500 is a treadmill.
**Cut for v1:** pivot tables, charts, conditional formatting, macros, external
data connections, array formulas.

### Workflow — *do last, and narrowly*

**Exists:** JetStream with durable consumers, retry and terminal paths is
*exactly* the right substrate for a step engine. This is a real head start.
**Missing:** DAG editor, credential vault, connector library, execution
sandbox.
**Hard part:** **running user-supplied code safely.** Multi-tenant + arbitrary
JS means isolates or microVMs (V8 isolates, gVisor, Firecracker). Getting this
wrong is a container escape, not a bug.
**Size:** internal-only automation = M. n8n parity = XL and never finished.
**Recommendation:** scope v1 to *triggers from SuperOps events → actions in
SuperOps*, plus a generic authenticated HTTP request node. No arbitrary code
execution. That covers most real internal automation and avoids the sandbox
problem entirely. Revisit code nodes only with a dedicated isolation story.

---

## 6. Sequence

```
Phase 0  spine ──────────────────────────────────────────────┐
         object permissions · collaboration layer · mail send │
         unified search · inbox · SSO · audit                 │
                                                              ▼
Phase 1  Drive + editor registry ──► every editor plugs in here
                                                              │
Phase 2  Work tracking ──► no new hard problem; richest seams │
                                                              │
Phase 3  Docs ───────────► first editor on the registry       │
                                                              │
Phase 4  Spreadsheet ────► second editor; reuses the grid of  │
                           work-tracking views                │
                                                              │
Phase 5  Design surface ─► third editor; heaviest client work │
                                                              │
   ·     Huddle ─────────► order-independent; slot in whenever │
                           capacity exists                    │
                                                              │
Phase 6  Email (shared inbox) ──► needs Drive for attachments │
                                                              │
Phase 7  Workflow ───────► triggers on everything above       ▼
```

Rationale for the order:

- **Drive first** because it is the cheapest pillar, the most depended-upon,
  and it carries the editor registry every Tier C pillar plugs into.
- **Work tracking second** because it introduces no new hard problem, produces
  the richest seams, and builds the comment surface the editors reuse.
- **The three editors are sequenced by risk, not by value.** Docs first because
  its document model is the best understood and it proves the collaboration
  layer against a real workload. Spreadsheet second because its hard part (the
  formula engine) is isolated and testable. The design surface last because it
  is the heaviest client-side build and benefits most from a proven substrate.
- **Huddle is order-independent** — it shares almost nothing with the rest and
  is the best value-per-effort of the nine once LiveKit is chosen. Schedule it
  whenever a team is free.
- **Email late** because its cost is operational and ongoing; it should start
  when the team can absorb an on-call surface.
- **Workflow last** because it is only valuable once there is something to
  automate, and it triggers on everything above it.

---

## 7. Explicit cuts

A roadmap without cuts is a wish list.

These are the guards that keep the nine pillars from becoming nine products.
Each one is a re-planning trigger, not a backlog item.

| Cut | Why |
|---|---|
| Pen tool, boolean path ops, plugins, prototyping | The line between a scoped design surface and a Figma-class engine (§5) |
| Spreadsheet: pivot tables, charts, macros, array formulas | ~50–80 functions covers real use; Excel's ~500 is a treadmill |
| Jira-style configurability | Custom fields and per-project workflows are Jira's cost, not its value |
| Personal mailboxes (replace Gmail) | Competes with clients people love, XL cost, no differentiation |
| n8n connector-library parity | 400+ integrations is a maintenance treadmill, not an engineering problem |
| Arbitrary code execution in workflows (v1) | Sandbox escape risk out of proportion to v1 value |
| Desktop file sync client | A product in itself |
| Huddle recording/transcription (v1) | Storage, cost and compliance surface |
| Native mobile *editing* for all three editors | See §3 — web-first until users ask |

---

## 8. Decisions needed before Phase 0 starts

1. **The RN fork in §3** — web-first, WebView, or split app?
2. **Deployment posture** — is this self-hosted-only, or will you run a hosted
   tier? It changes the permission model (single vs multi-org), the workflow
   sandbox requirement, and every capability in §3c. Note this is the one
   decision that does **not** block starting: §3c exists precisely so the
   answer can be per-deployment rather than baked in. It does decide which
   transports ship configured by default.
3. **Who is the first customer?** The order above optimizes for a company using
   this as their whole stack. If the near-term buyer wants one specific pillar,
   that pillar moves up regardless of leverage.

---

*Written 2026-07-25 against `d0a484b`. Nine pillars; revised live as scope was
added during the session — the design-surface sizing in §5 was re-priced once
the scope moved from Figma-class to a bounded surface, and the Docs CRDT cut
was withdrawn once a second and third editor needed multiplayer.*
