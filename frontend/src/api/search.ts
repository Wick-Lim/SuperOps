import { api } from './client'

export interface SearchResult {
  hits: Array<{ id: string; channel_id: string; workspace_id: string; user_id: string; content: string }>
  estimated_total: number
  processing_time_ms: number
}

export const searchApi = {
  search(workspaceId: string, query: string, opts?: { channel?: string; from?: string; limit?: number }) {
    const params = new URLSearchParams({ q: query })
    if (opts?.channel) params.set('channel', opts.channel)
    if (opts?.from) params.set('from', opts.from)
    if (opts?.limit) params.set('limit', String(opts.limit))
    return api.get<SearchResult>(`/workspaces/${workspaceId}/search?${params}`)
  },
}
