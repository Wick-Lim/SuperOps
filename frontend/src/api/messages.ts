import { api } from './client'
import type { Message, Reaction, ApiResponse } from '@/lib/types'

export const messageApi = {
  list(channelId: string, cursor?: string) {
    const params = cursor ? `?cursor=${cursor}&limit=50` : '?limit=50'
    return api.get<Message[]>(`/channels/${channelId}/messages${params}`) as Promise<ApiResponse<Message[]>>
  },
  send(channelId: string, content: string, parentId?: string) {
    return api.post<Message>(`/channels/${channelId}/messages`, {
      content,
      parent_id: parentId,
    })
  },
  edit(channelId: string, messageId: string, content: string) {
    return api.patch<Message>(`/channels/${channelId}/messages/${messageId}`, { content })
  },
  delete(channelId: string, messageId: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/messages/${messageId}`)
  },
  listThread(messageId: string) {
    return api.get<Message[]>(`/messages/${messageId}/thread`)
  },
  replyThread(messageId: string, content: string) {
    return api.post<Message>(`/messages/${messageId}/thread`, { content })
  },
  addReaction(channelId: string, messageId: string, emoji: string) {
    return api.post<Reaction>(`/channels/${channelId}/messages/${messageId}/reactions`, { emoji })
  },
  removeReaction(channelId: string, messageId: string, emoji: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/messages/${messageId}/reactions/${emoji}`)
  },
}
