import { api, withPaging } from './client'
import type { Bookmark, Message, Reaction } from '../lib/types'

export const messageApi = {
  /** Newest-first page; `messageStore.setMessages` reverses it into ascending order. */
  list(channelId: string, cursor?: string) {
    return api.get<Message[]>(withPaging(`/channels/${channelId}/messages`, cursor, 50))
  },
  send(
    channelId: string,
    content: string,
    opts?: { parentId?: string; fileIds?: string[]; scheduledAt?: string },
  ) {
    // JSON.stringify drops undefined values, so always spelling the optional
    // keys out is safe under DisallowUnknownFields.
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
    return api.del<{ message: string }>(
      `/channels/${channelId}/messages/${messageId}/reactions/${encodeURIComponent(emoji)}`,
    )
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
  /** Paginated; the cursor is keyed on `pinned_at`, not `created_at`. */
  listPins(channelId: string, cursor?: string, limit?: number) {
    return api.get<Message[]>(withPaging(`/channels/${channelId}/pins`, cursor, limit))
  },
  forward(channelId: string, messageId: string, targetChannelId: string) {
    return api.post<Message>(`/channels/${channelId}/messages/${messageId}/forward`, {
      target_channel_id: targetChannelId,
    })
  },
  bookmark(messageId: string) {
    return api.post<{ message: string }>(`/messages/${messageId}/bookmark`)
  },
  unbookmark(messageId: string) {
    return api.del<{ message: string }>(`/messages/${messageId}/bookmark`)
  },
  /** Paginated list of `{message, bookmarked_at}`, cursor keyed on `bookmarks.created_at`. */
  listBookmarks(cursor?: string, limit?: number) {
    return api.get<Bookmark[]>(withPaging('/bookmarks', cursor, limit))
  },
  /** Paginated replies to `messageId` (the root message is not included). */
  listThread(messageId: string, cursor?: string, limit?: number) {
    return api.get<Message[]>(withPaging(`/messages/${messageId}/thread`, cursor, limit))
  },
  replyThread(messageId: string, content: string) {
    return api.post<Message>(`/messages/${messageId}/thread`, { content })
  },
  /** Paginated; the cursor is keyed on `scheduled_at`. */
  listScheduled(channelId: string, cursor?: string, limit?: number) {
    return api.get<Message[]>(withPaging(`/channels/${channelId}/scheduled`, cursor, limit))
  },
  cancelScheduled(channelId: string, messageId: string) {
    return api.del<{ message: string }>(`/channels/${channelId}/scheduled/${messageId}`)
  },
}
