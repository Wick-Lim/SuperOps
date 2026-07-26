import * as Y from 'yjs'
import { Awareness, applyAwarenessUpdate, encodeAwarenessUpdate } from 'y-protocols/awareness'
import { wsManager, type RoomEvent } from '../websocket'
import { collabApi } from '../../api/collab'
import { fromBase64, toBase64 } from './base64'

/**
 * The SuperOps Yjs provider.
 *
 * It is deliberately NOT a feature of the docs editor. `y-prosemirror` takes a
 * `Y.Doc` and an `Awareness`, not a provider, so the transport binding is
 * editor-independent and the spreadsheet and the design surface import this
 * unchanged. Writing it inside one editor would be the first of the
 * three-implementations failures the registry exists to prevent.
 *
 * What it is responsible for:
 *
 *   - joining and leaving the room, and surviving a reconnect
 *   - fan-in: applying everyone's updates, including its OWN echo
 *   - fan-out: sending local updates, over the socket or over HTTP when they
 *     are too big for a frame
 *   - the watermark, advanced only over a CONTIGUOUS prefix
 *   - awareness (cursors), which is relayed and never stored
 *   - compaction on request
 *
 * What it is NOT responsible for: the document's meaning. It never inspects an
 * update, which is the same property the server has and for the same reason.
 */

/**
 * The socket refuses an update over 32 KiB (ws/client.go's
 * maxCollabPayloadBytes). A pasted table exceeds it easily, so the HTTP path is
 * not an exotic fallback — it is the normal route for a paste, and an editor
 * without it loses that paste silently.
 *
 * The threshold here is the BASE64 length, because that is what the frame
 * actually carries: encoding/json renders []byte as base64, so 32 KiB of JSON
 * is 24 KiB of update.
 */
const MAX_FRAME_UPDATE_BYTES = 24 * 1024

/** Local origin tag, so the provider can tell its own edits from applied ones. */
const LOCAL_ORIGIN = 'superops-local'

export type ProviderStatus = 'connecting' | 'synced' | 'read-only' | 'revoked' | 'error'

export interface ProviderOptions {
  documentId: string
  doc: Y.Doc
  /** The local user, for the awareness state other people render as a cursor. */
  user: { id: string; name: string; color: string }
  onStatus?: (status: ProviderStatus, detail?: string) => void
  /** The server has noticed this document's stored projection sitting behind
   * the log and is asking for a fresh one. It cannot produce one itself — it
   * never interprets a CRDT update — so this is the repair path for a document
   * whose last editor's browser died before it could flush. Ignored when this
   * client cannot write: the server would refuse the projection anyway. */
  onProjectRequest?: () => void
}

export class CollabProvider {
  readonly documentId: string
  readonly doc: Y.Doc
  readonly awareness: Awareness

  private status: ProviderStatus = 'connecting'
  private canWrite = false
  /**
   * The highest CONTIGUOUS sequence applied.
   *
   * Contiguous is the whole point. The server broadcasts after committing, so
   * seq 6 can reach a client before seq 5 — that is safe for the document,
   * because CRDT updates are order-independent, and fatal for a watermark: a
   * client that jumped to 6 would ask for state `since=6` on the next
   * reconnect and never receive 5, losing an edit permanently and invisibly.
   */
  private watermark = 0
  /** Out-of-order seqs held until the gap closes. */
  private pending = new Set<number>()
  private headSeq = 0

  private offRoom: (() => void) | null = null
  private updateHandler: ((update: Uint8Array, origin: unknown) => void) | null = null
  private awarenessHandler: ((changes: unknown, origin: unknown) => void) | null = null
  private destroyed = false
  private readonly onStatus?: (status: ProviderStatus, detail?: string) => void
  private readonly onProjectRequest?: () => void

  constructor(opts: ProviderOptions) {
    this.documentId = opts.documentId
    this.doc = opts.doc
    this.onStatus = opts.onStatus
    this.onProjectRequest = opts.onProjectRequest
    this.awareness = new Awareness(this.doc)
    this.awareness.setLocalStateField('user', opts.user)

    this.offRoom = wsManager.onRoom((e) => this.handleRoom(e))

    this.updateHandler = (update, origin) => {
      // Only LOCAL edits go back out. Re-broadcasting an update we just applied
      // would echo forever between two clients.
      if (origin === LOCAL_ORIGIN || this.destroyed) return
      void this.publish(update)
    }
    this.doc.on('update', this.updateHandler)

    this.awarenessHandler = (_changes, origin) => {
      if (origin === LOCAL_ORIGIN || this.destroyed) return
      this.publishAwareness()
    }
    this.awareness.on('update', this.awarenessHandler)

    wsManager.joinRoom(this.documentId)
  }

  /** Tears down every listener and leaves the room. Safe to call twice. */
  destroy() {
    if (this.destroyed) return
    this.destroyed = true
    if (this.updateHandler) this.doc.off('update', this.updateHandler)
    if (this.awarenessHandler) this.awareness.off('update', this.awarenessHandler)
    this.offRoom?.()
    this.offRoom = null
    // Announce the departure so other cursors disappear immediately rather than
    // lingering until an awareness timeout.
    this.awareness.setLocalState(null)
    this.awareness.destroy()
    wsManager.leaveRoom(this.documentId)
  }

  /** Whether the server said this connection may write. */
  get writable() {
    return this.canWrite
  }

  get currentStatus() {
    return this.status
  }

  // -------------------------------------------------------------------------
  // Room events
  // -------------------------------------------------------------------------

  private handleRoom(e: RoomEvent) {
    if (e.documentId !== this.documentId || this.destroyed) return
    switch (e.kind) {
      case 'joined':
        this.canWrite = e.canWrite
        this.headSeq = e.headSeq
        void this.catchUp()
        break
      case 'resumed':
        // Nothing published while the socket was down is replayed, so the only
        // recovery is to re-read the log from the watermark. The join ack that
        // follows will do it; this exists so a provider whose room was restored
        // without a fresh `joined` still reconciles.
        this.setStatus('connecting')
        break
      case 'left':
        if (e.reason === 'revoked') {
          this.canWrite = false
          this.setStatus('revoked', 'Your access to this document was removed.')
        }
        break
      case 'update':
        this.applyRemote(e.seq, e.update, e.originConn)
        break
      case 'awareness':
        this.applyAwareness(e.state, e.actorId)
        break
      case 'compact':
        void this.compact(e.headSeq)
        break
      case 'project':
        // Only a writer answers. Every replica holding a member of this room
        // asks its own leader, so several clients may answer one request —
        // harmless, because the server's monotonic guard keeps the first and
        // discards the rest.
        if (this.canWrite) this.onProjectRequest?.()
        break
    }
  }

  // -------------------------------------------------------------------------
  // Fan-in
  // -------------------------------------------------------------------------

  /**
   * Applies one relayed update.
   *
   * The origin connection receives its OWN echo — that is how it learns the seq
   * its update was assigned — and the update is applied anyway. Applying an
   * update twice is a no-op in Yjs, and skipping the echo would leave the
   * watermark with a permanent hole exactly where this client's own edits are.
   */
  private applyRemote(seq: number, updateBase64: string, _originConn: string) {
    try {
      Y.applyUpdate(this.doc, fromBase64(updateBase64), LOCAL_ORIGIN)
    } catch {
      // A malformed update cannot be recovered from by retrying, and dropping
      // the document would lose local edits. Refetching state is the honest
      // repair.
      void this.catchUp()
      return
    }
    this.advanceWatermark(seq)
  }

  private advanceWatermark(seq: number) {
    if (seq <= this.watermark) return
    this.pending.add(seq)
    while (this.pending.has(this.watermark + 1)) {
      this.pending.delete(this.watermark + 1)
      this.watermark += 1
    }
    if (seq > this.headSeq) this.headSeq = seq

    // A gap that has not closed after a while means the missing update was not
    // merely reordered — it was dropped by backpressure, and nothing replays
    // it. Refetch rather than sit behind a hole forever.
    if (this.pending.size > 0 && seq - this.watermark > 64) {
      void this.catchUp()
    }
  }

  /**
   * Reads the log from the watermark over HTTP.
   *
   * Also the initial load: a client that joins a document it has never seen has
   * watermark 0 and gets the snapshot plus the tail, which is exactly what a
   * reconnect after a long absence gets. One path, so the rare case is the one
   * that is exercised constantly.
   */
  private async catchUp(): Promise<void> {
    if (this.destroyed) return
    try {
      let since = this.watermark
      for (;;) {
        const res = await collabApi.state(this.documentId, since)
        const state = res.data
        if (!state) break

        if (state.snapshot) {
          Y.applyUpdate(this.doc, fromBase64(state.snapshot), LOCAL_ORIGIN)
        }
        for (const u of state.updates ?? []) {
          Y.applyUpdate(this.doc, fromBase64(u.payload), LOCAL_ORIGIN)
        }

        // The watermark jumps to through_seq wholesale, which is legitimate
        // precisely because this response IS contiguous — the server read the
        // log in order. Everything buffered below it is now redundant.
        this.watermark = Math.max(this.watermark, state.through_seq)
        this.headSeq = Math.max(this.headSeq, state.head_seq)
        for (const seq of [...this.pending]) {
          if (seq <= this.watermark) this.pending.delete(seq)
        }

        if (!state.has_more) break
        since = state.through_seq
      }
      this.setStatus(this.canWrite ? 'synced' : 'read-only')
      // Announce this client's cursor once it is caught up. The server sends no
      // roster by design, so the protocol is that a client re-announces itself
      // whenever it sees somebody it does not know — and on join, nobody does.
      this.publishAwareness()
    } catch (err) {
      this.setStatus('error', err instanceof Error ? err.message : 'Could not load the document')
    }
  }

  private applyAwareness(stateBase64: string, actorId: string) {
    try {
      const known = this.awareness.getStates().size
      applyAwarenessUpdate(this.awareness, fromBase64(stateBase64), 'remote')
      // The re-announce rule: there is no roster, so somebody appearing for the
      // first time is how this client learns to introduce itself back.
      if (this.awareness.getStates().size > known && actorId) {
        this.publishAwareness()
      }
    } catch {
      /* an unreadable cursor is a cursor that does not render; never fatal */
    }
  }

  // -------------------------------------------------------------------------
  // Fan-out
  // -------------------------------------------------------------------------

  private async publish(update: Uint8Array): Promise<void> {
    if (!this.canWrite) return
    const encoded = toBase64(update)
    if (encoded.length <= MAX_FRAME_UPDATE_BYTES) {
      wsManager.sendCollabUpdate(this.documentId, encoded)
      return
    }
    // Too large for a frame — a paste, or a backlog flushed on reconnect. The
    // socket would refuse it outright, so this is the only route it has.
    try {
      await collabApi.append(this.documentId, encoded)
    } catch (err) {
      this.setStatus('error', err instanceof Error ? err.message : 'An edit could not be sent')
    }
  }

  private publishAwareness() {
    if (this.destroyed) return
    try {
      const update = encodeAwarenessUpdate(this.awareness, [this.doc.clientID])
      wsManager.sendAwareness(this.documentId, toBase64(update))
    } catch {
      /* awareness is best effort by construction */
    }
  }

  /**
   * Answers the server's request for a snapshot.
   *
   * The server cannot build one — it never interprets an update — so this is
   * the entire compaction mechanism, and a long-lived document whose clients
   * ignore it grows its log forever.
   */
  private async compact(throughSeq: number): Promise<void> {
    if (this.destroyed || !this.canWrite) return
    try {
      const snapshot = Y.encodeStateAsUpdate(this.doc)
      await collabApi.snapshot(this.documentId, throughSeq, toBase64(snapshot))
    } catch {
      // Losing the race to another client is normal and not an error worth
      // showing anybody; so is a transient failure, because the server asks
      // again the next time the threshold is crossed.
    }
  }

  private setStatus(status: ProviderStatus, detail?: string) {
    if (this.status === status) return
    this.status = status
    this.onStatus?.(status, detail)
  }
}
