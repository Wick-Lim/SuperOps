import { api } from './client'
import type { Channel, ChannelMemberView } from '../lib/types'

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
  update(workspaceId: string, channelId: string, data: { name?: string; description?: string; topic?: string }) {
    return api.patch<Channel>(`/workspaces/${workspaceId}/channels/${channelId}`, data)
  },
  createDM(workspaceId: string, userIds: string[]) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/dm`, { user_ids: userIds })
  },
  join(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/join`)
  },
  leave(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/leave`)
  },
  archive(workspaceId: string, channelId: string) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/${channelId}/archive`)
  },
  unarchive(workspaceId: string, channelId: string) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/${channelId}/unarchive`)
  },
  listMembers(workspaceId: string, channelId: string) {
    return api.get<ChannelMemberView[]>(`/workspaces/${workspaceId}/channels/${channelId}/members`)
  },
  markRead(channelId: string) {
    return api.put<{ message: string }>(`/channels/${channelId}/read`)
  },
  typing(channelId: string) {
    return api.get<{ typing: string[] }>(`/channels/${channelId}/typing`)
  },
}
