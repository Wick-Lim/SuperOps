import { api } from './client'
import type { Channel } from '../lib/types'

export const channelApi = {
  list(workspaceId: string) {
    return api.get<Channel[]>(`/workspaces/${workspaceId}/channels`)
  },
  create(workspaceId: string, data: { name: string; slug: string; description?: string; type?: string }) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels`, data)
  },
  join(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/join`)
  },
  markRead(channelId: string) {
    return api.put<{ message: string }>(`/channels/${channelId}/read`)
  },
}
