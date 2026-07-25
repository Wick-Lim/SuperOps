import { api, withPaging } from './client'
import type { AppNotification } from '../lib/types'

export const notificationApi = {
  /** Paginated (`meta.cursor` / `meta.has_more`). */
  list(cursor?: string, limit?: number) {
    return api.get<AppNotification[]>(withPaging('/notifications', cursor, limit))
  },
  markRead(id: string) {
    return api.put<{ message: string }>(`/notifications/${id}/read`)
  },
  markAllRead() {
    return api.put<{ message: string }>('/notifications/read-all')
  },
  unreadCount() {
    return api.get<{ count: number }>('/notifications/unread-count')
  },
}
