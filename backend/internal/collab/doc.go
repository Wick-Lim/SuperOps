// Package collab is the shared collaboration layer: one implementation of
// multi-user editing that the block editor, the spreadsheet and the design
// surface all sit on (ROADMAP §3b, §4).
//
// # The server is an opaque relay
//
// The CRDT lives in the client — Yjs. This package stores and fans out update
// blobs without ever interpreting one. There is no Go CRDT, no server-side
// merge and no validation of an update's contents beyond its size. That is
// deliberate: a second implementation that disagreed with the client's about
// any edge case would produce a corrupted document that neither side could be
// debugged from, and it would have to be kept in lockstep with the client
// library forever. The cost of the choice is stated plainly below under
// "trust".
//
// # Persistence
//
// An append-only log in Postgres (collab_updates), periodically compacted into
// a snapshot (collab_snapshots). Loading a document is "the newest snapshot,
// plus every update after it".
//
// Ordering is what makes the log usable, and it comes from a counter on the
// document row rather than from a sequence:
//
//	UPDATE collab_documents SET head_seq = head_seq + 1 ... RETURNING head_seq
//
// as the first statement of the append transaction. The row lock serialises
// appends to one document, so seq order is commit order and a reader that sees
// seq N knows every seq below N is committed and visible. A SEQUENCE would give
// 5 to a transaction that commits after the one holding 6, and a reader that
// saw 6 would conclude 5 does not exist. The cost is that appends to one
// document are serialised; they are single-row inserts of a few hundred bytes,
// and the alternative is a log with holes.
//
// # Compaction cannot lose a concurrent write
//
// Compaction takes the same document row lock (SELECT ... FOR UPDATE) before it
// reads head_seq and before it deletes anything. An append that has not
// committed is therefore an append the compactor is blocked behind — it cannot
// delete an update that is still in flight, and it cannot observe a head_seq
// that a not-yet-committed append will fill in. A compactor whose through_seq
// is no longer the head loses the race harmlessly: the transaction refuses it
// as stale and nothing is deleted.
//
// The snapshot itself is produced by a *client*, because the server cannot
// merge CRDT state. When a document's log grows past the threshold the server
// asks one connected client (per replica, chosen deterministically) for a
// snapshot over the socket; the client posts it back over HTTP. If nobody is
// connected the log does not grow either, because updates only come from
// connected clients — the log is bounded by activity, not by time.
//
// # Reconnect
//
// A client that has been offline has both local changes the server never saw
// and remote changes it never saw. The protocol answers each separately:
//
//   - Missed remote changes: the client keeps a watermark — the highest seq
//     below which it has received *every* update. On reconnect it rejoins the
//     room (collab.join returns head_seq) and, if it is behind, calls
//     GET /collab/documents/{id}/state?since={watermark} until has_more is
//     false. The watermark must only ever advance over a contiguous prefix:
//     two appends commit in seq order but their fan-outs race, so seq 6 can
//     arrive before seq 5, and a watermark that jumped to 6 would lose 5.
//
//   - Its own unsent changes: the client replays them. Yjs updates are
//     idempotent and order-independent, so replaying one the server already
//     has is a no-op. A client that lost its pending queue (a page reload)
//     posts Y.encodeStateAsUpdate(doc) instead — a full local state is itself
//     a valid update, and merging it costs correctness nothing.
//
// Rejoining before loading is the right order: anything that arrives during the
// load is applied on top, and a CRDT does not care in which order it gets them.
//
// # Awareness
//
// Cursors, selections and who-is-here ride the same socket and never touch
// Postgres. There is no awareness table in migration 015 on purpose — a table
// would immediately be written to by every mouse move. Awareness needs read
// access only: a viewer's cursor is exactly as visible as an editor's.
//
// # Bounds
//
// Four of them, because "the server holds an opaque blob a client chose" is
// exactly the shape that exhausts memory:
//
//   - One update over the socket: 32 KiB decoded (ws.maxCollabPayloadBytes),
//     inside a 64 KiB frame limit. A keystroke batch is tens of bytes.
//   - One update over HTTP: MaxUpdateBytes (1 MiB), also a CHECK constraint, so
//     a bug in the handler cannot get a larger row past Postgres. This is the
//     path for a large paste or an offline backlog.
//   - One snapshot: MaxSnapshotBytes (16 MiB), also a CHECK.
//   - Frame rate: collaboration frames have their own token bucket per
//     connection (ws.collabRatePerSecond), so an editing session is not charged
//     to the 5/s chat budget and cannot use the larger frames to flood.
//
// A document's total size is bounded by compaction, which is what stops a
// long-lived document accumulating an unbounded log.
//
// # Access control and trust
//
// Joining a room is an authorization decision and goes through internal/authz
// like every other one. Today that is workspace membership with guests
// read-only (WorkspaceAuthorizer); when object-level ACLs land (ROADMAP §4)
// the Authorizer interface is the one place that changes.
//
// Access withdrawn mid-session is handled by Hub.RevokeRoom (immediate,
// cluster-wide) with the socket's periodic recheck as the backstop. The
// keystroke path itself does not re-query authorization — membership and the
// write flag are recorded at join time — which is the same trade the typing
// indicator makes and the reason revocation has to be pushed rather than
// polled.
//
// The trust boundary is worth stating: any client authorized to write can
// already corrupt a document by sending a malformed update, and a snapshot lets
// it do so in one shot. That is inherent to putting the CRDT in the client. The
// mitigation is that snapshots are kept as a short history rather than
// overwritten, so the previous good state is still on disk.
package collab
