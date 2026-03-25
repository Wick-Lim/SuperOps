import { api } from './client'
import type { Channel, ChannelMember } from '@/lib/types'

export const channelApi = {
  list(workspaceId: string) {
    return api.get<Channel[]>(`/workspaces/${workspaceId}/channels`)
  },
  browse(workspaceId: string) {
    return api.get<Channel[]>(`/workspaces/${workspaceId}/channels/browse`)
  },
  get(workspaceId: string, channelId: string) {
    return api.get<Channel>(`/workspaces/${workspaceId}/channels/${channelId}`)
  },
  create(workspaceId: string, data: { name: string; slug: string; description?: string; type?: string }) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels`, data)
  },
  join(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/join`)
  },
  leave(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/leave`)
  },
  listMembers(workspaceId: string, channelId: string) {
    return api.get<ChannelMember[]>(`/workspaces/${workspaceId}/channels/${channelId}/members`)
  },
  markRead(channelId: string) {
    return api.put<{ message: string }>(`/channels/${channelId}/read`)
  },
}
