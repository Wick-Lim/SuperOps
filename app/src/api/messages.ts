import { api } from './client'
import type { Message, Reaction, ApiResponse } from '../lib/types'

export const messageApi = {
  list(channelId: string, cursor?: string) {
    const params = cursor ? `?cursor=${encodeURIComponent(cursor)}&limit=50` : '?limit=50'
    return api.get<Message[]>(`/channels/${channelId}/messages${params}`) as Promise<ApiResponse<Message[]>>
  },
  send(channelId: string, content: string, opts?: { parentId?: string; fileIds?: string[]; scheduledAt?: string }) {
    return api.post<Message>(`/channels/${channelId}/messages`, {
      content,
      parent_id: opts?.parentId,
      file_ids: opts?.fileIds,
      scheduled_at: opts?.scheduledAt,
    })
  },
  edit(channelId: string, messageId: string, content: string) {
    return api.patch<Message>(`/channels/${channelId}/messages/${messageId}`, { content })
  },
  del(channelId: string, messageId: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/messages/${messageId}`)
  },
  react(channelId: string, messageId: string, emoji: string) {
    return api.post<Reaction>(`/channels/${channelId}/messages/${messageId}/reactions`, { emoji })
  },
  unreact(channelId: string, messageId: string, emoji: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/messages/${messageId}/reactions/${encodeURIComponent(emoji)}`)
  },
  getReactions(messageId: string) {
    return api.get<Reaction[]>(`/messages/${messageId}/reactions`)
  },
  pin(channelId: string, messageId: string) {
    return api.post<{ message: string }>(`/channels/${channelId}/messages/${messageId}/pin`)
  },
  unpin(channelId: string, messageId: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/messages/${messageId}/pin`)
  },
  listPins(channelId: string) {
    return api.get<Message[]>(`/channels/${channelId}/pins`)
  },
  forward(channelId: string, messageId: string, targetChannelId: string) {
    return api.post<Message>(`/channels/${channelId}/messages/${messageId}/forward`, { target_channel_id: targetChannelId })
  },
  bookmark(messageId: string) {
    return api.post<{ message: string }>(`/messages/${messageId}/bookmark`)
  },
  unbookmark(messageId: string) {
    return api.del<{ message: string }>(`/messages/${messageId}/bookmark`)
  },
  listBookmarks() {
    return api.get<Array<{ id: string; channel_id: string; user_id: string; content: string; created_at: string }>>('/bookmarks')
  },
  listThread(messageId: string) {
    return api.get<Message[]>(`/messages/${messageId}/thread`)
  },
  replyThread(messageId: string, content: string) {
    return api.post<Message>(`/messages/${messageId}/thread`, { content })
  },
  listScheduled(channelId: string) {
    return api.get<Message[]>(`/channels/${channelId}/scheduled`)
  },
  cancelScheduled(channelId: string, messageId: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/scheduled/${messageId}`)
  },
}
