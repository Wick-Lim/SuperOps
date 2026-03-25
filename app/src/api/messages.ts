import { api } from './client'
import type { Message, ApiResponse } from '../lib/types'

export const messageApi = {
  list(channelId: string, cursor?: string) {
    const params = cursor ? `?cursor=${cursor}&limit=50` : '?limit=50'
    return api.get<Message[]>(`/channels/${channelId}/messages${params}`) as Promise<ApiResponse<Message[]>>
  },
  send(channelId: string, content: string, parentId?: string) {
    return api.post<Message>(`/channels/${channelId}/messages`, { content, parent_id: parentId })
  },
  listThread(messageId: string) {
    return api.get<Message[]>(`/messages/${messageId}/thread`)
  },
  replyThread(messageId: string, content: string) {
    return api.post<Message>(`/messages/${messageId}/thread`, { content })
  },
}
