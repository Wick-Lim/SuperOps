import { api } from './client'
import type { AppNotification } from '../lib/types'

export const notificationApi = {
  list(cursor?: string) {
    const params = cursor ? `?cursor=${encodeURIComponent(cursor)}` : ''
    return api.get<AppNotification[]>(`/notifications${params}`)
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
