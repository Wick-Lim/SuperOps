# SuperOps Hardening and Completion Program

**Status:** Approved in conversation on 2026-08-01  
**Scope owner:** Project maintainer  
**Implementation strategy:** Sequential, test-gated sub-projects on an isolated branch/worktree

## Objective

Resolve the defects and incomplete product paths identified by the full repository audit, in the agreed priority order, without trading data safety or tenant isolation for feature breadth.

The program finishes when every phase below satisfies its own acceptance gate and the final real-infrastructure and browser/device verification suite passes.

## Fixed decisions

- Work proceeds in six ordered phases. A later phase does not start until the preceding phase has passed its focused tests and the relevant broader regression suites.
- Every behavior change and bug fix follows red-green-refactor TDD. Configuration-only changes receive render, syntax, or smoke tests that fail before the change whenever the repository can express one.
- Implementation occurs in an isolated worktree and a `codex/` branch, not directly on `main`.
- Existing public API behavior remains backward compatible unless the current behavior is unsafe or non-functional. Any intentional incompatibility is documented in the relevant phase design.
- No new external runtime dependency is added unless the phase design demonstrates that existing Go, Expo, React Native, Yjs, PostgreSQL, NATS, Redis, and Helm facilities cannot satisfy the requirement.
- Collaborative editing is fail-closed during a connection outage in Phase 1. This phase does not introduce a persistent offline document cache. Edits that occur in the send/disconnect race are retained in memory and recovered before the UI reports `synced` again.
- Security and correctness gates are mandatory. Product-completion work does not bypass or weaken them.

## Program decomposition

### Phase 1 — Client data safety

1. Clear every account-scoped client store, cache, subscription, and realtime listener on logout or terminal authentication failure.
2. Stop collaborative editors from claiming changes are saved while realtime delivery is unavailable.
3. Retain an update produced in the send/disconnect race and flush it through the durable HTTP append path before returning to `synced`.

### Phase 2 — Core correctness and authorization

1. Drive projections use the provider's contiguous durable sequence rather than the file descriptor's one-time head value.
2. Refresh tokens are consumed atomically, so one stored token can produce at most one successor session.
3. Removing a workspace member revokes workspace-scoped delivery on every API replica, not only channel subscriptions on the handling replica.
4. A Drive folder's domain parent and ACL path move in the same PostgreSQL transaction.

### Phase 3 — Migration and deployment contract

1. New migration numbers are derived from the highest numeric prefix, with duplicate and retrograde migrations rejected in CI.
2. Helm's default application tag matches the tags produced by the release workflow.
3. Bundled MinIO and external S3 are two valid, rendered, documented configurations with correct enablement and credentials.
4. The frontend permissions policy permits same-origin microphone capture required by web huddles.

### Phase 4 — Event durability

1. Message-side domain events are recorded transactionally and republished until JetStream acknowledges them.
2. Workflow effects distinguish reservation, execution, success, and failure. A crash before execution cannot be reported as a successful deduplicated action.

### Phase 5 — Product completion

1. Share links resolve to a short-lived, scoped credential that the authorization path actually consumes.
2. Drive exposes upload, new-version, and user/group grant flows in the shipped client.
3. Search routes issue results to `IssueDetail` and restores all information needed by collaborative-document deep links.
4. The shipped client consumes the unified inbox while retaining compatibility for legacy notification endpoints during migration.

Each item in Phase 5 receives a focused sub-design before implementation because credential semantics, upload UX, navigation, and inbox migration are independent product decisions.

### Phase 6 — End-to-end verification

1. Account A to logout to account B isolation.
2. Collaborative edit during a controlled socket interruption and recovery.
3. Concurrent refresh rotation and workspace-removal realtime revocation.
4. Existing-database migration upgrade plus fresh `up -> down -> up` replay.
5. Default Helm render and external S3 render.
6. Browser huddle microphone smoke test over HTTPS.
7. Full Postgres, Redis, NATS JetStream, MinIO/S3, and Meilisearch integration suite with zero skipped infrastructure tests.

## Phase 1 detailed design

### Session reset boundary

A single account-session reset operation owns cleanup. Logout, refresh failure that ends the session, and a terminal bootstrap authentication rejection (HTTP 401/403, not a transient network or server failure) call the same operation rather than clearing stores independently.

The reset operation is idempotent and performs these actions:

1. Reset the WebSocket manager, including reconnect history, subscriptions, room listeners, huddle listeners, event handlers, typing timers, and connection identifiers.
2. Clear workspace, channel, message, Drive, public-user, and UI state, including active channel and active thread references.
3. Clear module-level account caches such as direct-message rosters, custom emoji, workspace-role lookups, and failed user lookup attempts.
4. Leave deployment configuration and non-account UI preferences intact.

Logout captures the tokens required for best-effort push deregistration and server-side refresh-token revocation first. Local authentication and all account-scoped state are then cleared regardless of network failures. Remote cleanup can fail without resurrecting local data.

#### Session-reset invariants

- Once `isAuthenticated` becomes false, no selector can return data from the previous account.
- A second reset produces the same empty state and does not throw.
- A failed push deregistration, storage deletion, or server logout never prevents local cleanup.
- A new login begins with no active channel, thread, room, presence map, message page, Drive selection, or account-scoped cache from the previous login.

### Collaborative connection state

The WebSocket send primitive reports whether a frame was accepted by an open socket. The collaborative provider also subscribes to connection status instead of assuming that a successful initial catch-up remains valid forever.

State transitions are:

```text
construct -> connecting -> synced/read-only
synced -> connecting                 socket closes
synced -> saving                     local update is being delivered
saving -> synced                     own durable echo closes the pending set
saving -> connecting                 socket closes before acknowledgement
connecting/saving -> error           recovery append fails
connecting/error -> synced/read-only catch-up and pending flush succeed
any state -> revoked                 server revokes document access
```

The screen is editable in `synced` and `saving`, so normal typing does not pause for a network round trip. It becomes read-only in `connecting`, `error`, `read-only`, and `revoked`. The header displays `All changes saved` only in `synced`; `saving` has an explicit unsaved/saving label.

### Send/disconnect race recovery

The provider keeps a merged in-memory Yjs update containing local changes that are not yet known to be durable. Yjs updates are idempotent, so replaying a change that reached PostgreSQL but whose echo was lost is safe for document state.

For every local document update:

1. Merge it into the provider's pending update before attempting realtime send.
2. Mark the provider `saving`.
3. Send through WebSocket when the socket is open.
4. Treat an own-origin durable echo as acknowledgement. When every pending send has been acknowledged without a newer local update, discard the merged pending update and return to `synced`.

When the socket closes or a send is rejected:

1. Keep the merged pending update in memory.
2. Enter `connecting`, which disables editing.
3. On reconnect, fetch and apply the contiguous server state first.
4. Append the merged pending update through the existing authenticated HTTP append endpoint.
5. Apply the returned sequence as the provider's durable watermark, clear the pending update, and only then return to `synced`.

If the HTTP recovery fails, the document remains open but non-editable with a visible error and retry path. Because Phase 1 intentionally does not persist document bodies locally, closing the application after that explicit error may discard the in-memory pending update; the UI must never describe that state as saved.

### Projection sequence ownership

The collaborative provider is the source of truth for the highest contiguous sequence represented by the local Y.Doc. The file descriptor's `head_seq` is only an initial repair hint.

The provider emits a projection-watermark callback when:

- initial HTTP catch-up advances the contiguous watermark;
- an ordered remote update closes the next sequence;
- an own update receives its durable echo; or
- HTTP recovery append succeeds.

`CollabDocumentScreen` stores this watermark and passes it to the document, sheet, and design projection extractors. A projection is never published with a sequence above the contiguous local watermark. This preserves the backend's monotonic guard while allowing every later edit to advance searchable text, outline, and backlinks.

### Phase 1 error handling

- Logout cleanup is local-first and idempotent; remote failures are non-blocking.
- A collaborative send failure changes visible state immediately.
- A failed recovery does not clear the pending update and does not re-enable editing.
- Access revocation wins over reconnect or recovery and discards the ability to publish.
- Destroying a provider unregisters connection listeners and prevents later async recovery from changing screen state.

### Phase 1 tests

Tests exercise behavior rather than source-text patterns:

1. Populate every account store and cache, invoke session reset, and assert all account data and realtime registrations are absent.
2. Simulate account A logout followed by account B bootstrap and assert account A's active channel, messages, thread, users, Drive state, and presence cannot render or be selected.
3. Disconnect the fake socket, attempt a local Yjs update, and assert the provider leaves `synced`, the editor becomes non-editable, and the update remains pending.
4. Reconnect, return a server catch-up, and assert the pending update is appended over HTTP before `synced` is emitted.
5. Fail the recovery append and assert the provider remains non-editable with an error and retains the pending update for retry.
6. Advance sequences through remote and own echoes and assert projections use the new contiguous watermark; a sequence gap must not advance projection state.
7. Run all existing app tests and TypeScript checking after the focused tests.

### Phase 1 acceptance gate

- All focused tests demonstrate a red-green cycle.
- `npm test` and `npm run typecheck` pass with no unexpected warnings.
- A manual browser check confirms that disconnecting the WebSocket removes the saved indicator and disables all three collaborative surfaces.
- Git diff contains no unrelated UI or backend changes.

## Later-phase design constraints

### Phase 2

- Refresh consumption uses one SQL statement or one row-locked transaction and checks affected rows.
- Workspace revocation is cross-replica and cannot depend on the handling process owning the user's socket.
- Drive move uses the existing transaction-aware ACL move primitive and locks the domain rows needed for cycle and depth correctness.
- Every concurrency or rollback defect has a regression test that proves the previous interleaving fails.

### Phase 3

- CI tests both a fresh database and an already-upgraded database receiving one newly generated migration.
- Helm tests render default bundled infrastructure and external service variants independently.
- Secrets never move into ConfigMaps or rendered NOTES.
- HTTPS remains the production default; microphone permission is scoped to `self`, not globally enabled.

### Phase 4

- Outbox records share the business transaction and have stable deduplication identifiers.
- Publishers are safe under redelivery and expose backlog/failure metrics.
- Workflow providers receive stable idempotency keys. Internal state never equates `claimed` with `performed`.

### Phase 5

- Share credentials are short-lived, object-scoped, capability-limited, revocable, and excluded from ordinary logs.
- New UI paths use existing responsive and accessibility primitives and are verified at compact and wide breakpoints.
- Legacy APIs are removed only after the shipped client no longer depends on them.

### Phase 6

- No infrastructure-backed test may skip.
- Verification uses production-equivalent TLS, NATS JetStream, storage, search, and database settings.
- Completion claims quote the fresh command output and failure counts.

## Program risks and controls

| Risk | Control |
|---|---|
| A broad patch obscures regressions | Phase-specific plans, commits, reviews, and full regression gates |
| Parallel workers edit shared composition roots | Implementation tasks are sequential when files or interfaces overlap |
| Reliability changes create duplicate events | Stable idempotency keys plus redelivery tests |
| Offline recovery weakens local data security | Phase 1 uses memory-only pending state and explicit fail-closed UI |
| Helm fixes work only for one topology | Default and external-service render matrices in CI |
| Product completion expands without bound | Each Phase 5 feature gets its own approved sub-design and explicit non-goals |

## Completion definition

The program is complete only when all six phases have an accepted implementation, their focused and regression tests pass, the final infrastructure and browser/device verification has been run without skips, documentation matches the shipped behavior, and the working tree contains no unrelated changes.
