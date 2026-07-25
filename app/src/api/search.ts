import { api } from './client'

/** backend/internal/search/service.go — MessageDoc. */
export interface SearchHit {
  id: string
  channel_id: string
  workspace_id: string
  user_id: string
  content: string
  /** Unix seconds, NOT an RFC3339 string — the index stores a sortable integer. */
  created_at: number
  is_deleted?: boolean
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
    opts?: { channel?: string; from?: string; limit?: number },
  ) {
    const params = new URLSearchParams({ q: query })
    if (opts?.channel) params.set('channel', opts.channel)
    if (opts?.from) params.set('from', opts.from)
    if (opts?.limit) params.set('limit', String(opts.limit))
    return api.get<SearchResult>(`/workspaces/${workspaceId}/search?${params.toString()}`)
  },
}
