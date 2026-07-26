import { api } from './client'

/**
 * What the index can return. `message` is one of six, not the only one.
 *
 * The client declared only the message fields and never sent `type`, and the
 * server reads an absent `type` as EVERY type — so Drive documents, sheets,
 * designs and plain files came back and were rendered as chat messages: "Message
 * from <uploader> in #other channel", with the document body as the message
 * text and the file's real name nowhere. Tapping one asked for a channel whose
 * id was the empty string and dead-ended in "That channel is no longer
 * available."
 */
export type SearchHitType = 'message' | 'file' | 'document' | 'spreadsheet' | 'design' | 'issue'

/** backend/internal/search/service.go — Hit. */
export interface SearchHit {
  id: string
  /** Which kind of object. Absent from older servers; treat that as 'message'. */
  type?: SearchHitType
  channel_id: string
  workspace_id: string
  user_id: string
  /** The object's name. Empty for a message, which has no title. */
  title?: string
  content: string
  /** Unix seconds, NOT an RFC3339 string — the index stores a sortable integer. */
  created_at: number
  is_deleted?: boolean
}

/** True when a hit is a Drive object rather than a chat message. */
export function isDriveHit(hit: SearchHit): boolean {
  return hit.type !== undefined && hit.type !== 'message' && hit.type !== 'issue'
}

export interface SearchResult {
  hits: SearchHit[]
  estimated_total: number
  processing_time_ms: number
}

export const searchApi = {
  /**
   * Full-text search across the channels the caller can read. `channel` narrows
   * that set (403 for a channel they cannot read); `from` must be a user id.
   */
  messages(
    workspaceId: string,
    query: string,
    opts?: { channel?: string; from?: string; limit?: number; types?: SearchHitType[] },
  ) {
    const params = new URLSearchParams({ q: query })
    if (opts?.channel) params.set('channel', opts.channel)
    if (opts?.from) params.set('from', opts.from)
    if (opts?.limit) params.set('limit', String(opts.limit))
    // Absent means EVERY type on the server, which is why this exists.
    if (opts?.types?.length) params.set('type', opts.types.join(','))
    return api.get<SearchResult>(`/workspaces/${workspaceId}/search?${params.toString()}`)
  },
}
