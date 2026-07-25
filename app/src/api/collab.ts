import { api } from './client'

/**
 * The collaboration layer's HTTP surface.
 *
 * The socket carries the live stream; these are the three things it cannot do.
 * State is the bulk read — a snapshot plus the tail — which is both the initial
 * load and the reconnect recovery, because nothing published while a socket was
 * down is replayed. Append is the bulk write, for an update too large for a
 * 32 KiB frame; a pasted table exceeds that easily, so this is the normal route
 * for a paste rather than an exotic fallback. Snapshot is the entire compaction
 * mechanism: the server never interprets an update, so it cannot build one.
 */

export interface CollabUpdate {
  seq: number
  /** Opaque CRDT bytes, base64. Nothing on either side interprets them. */
  payload: string
  actor_id: string | null
  created_at: string
}

export interface CollabState {
  document_id: string
  /** Absent when the caller's watermark is already at or past the snapshot. */
  snapshot?: string
  snapshot_seq: number
  updates: CollabUpdate[]
  /** What the watermark becomes once everything here is applied. */
  through_seq: number
  /** The log head at read time; already stale by the time it arrives. */
  head_seq: number
  /** The response was truncated; ask again with since = through_seq. */
  has_more: boolean
}

export const collabApi = {
  /**
   * Reads the log from `since`.
   *
   * `since` is a CONTIGUOUS watermark, never the highest seq seen. The server
   * broadcasts after committing, so seq 6 can arrive before seq 5; asking for
   * state since 6 would skip 5 permanently and invisibly.
   */
  state(documentId: string, since: number) {
    return api.get<CollabState>(`/collab/documents/${documentId}/state?since=${since}`)
  },

  append(documentId: string, updateBase64: string) {
    return api.post<{ seq: number }>(`/collab/documents/${documentId}/updates`, {
      update: updateBase64,
    })
  },

  snapshot(documentId: string, throughSeq: number, snapshotBase64: string) {
    return api.post<{ snapshot_seq: number }>(`/collab/documents/${documentId}/snapshot`, {
      through_seq: throughSeq,
      snapshot: snapshotBase64,
    })
  },
}
