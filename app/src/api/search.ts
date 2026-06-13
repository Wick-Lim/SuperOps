import { api } from './client'

export interface SearchHit {
  id: string
  channel_id: string
  workspace_id: string
  user_id: string
  content: string
}

export interface SearchResult {
  hits: SearchHit[]
  estimated_total: number
  processing_time_ms: number
}

export const searchApi = {
  messages(workspaceId: string, query: string, opts?: { channel?: string; from?: string }) {
    const params = new URLSearchParams({ q: query })
    if (opts?.channel) params.set('channel', opts.channel)
    if (opts?.from) params.set('from', opts.from)
    return api.get<SearchResult>(`/workspaces/${workspaceId}/search?${params.toString()}`)
  },
}
