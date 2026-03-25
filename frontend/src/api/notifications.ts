import { api } from './client'

export interface Notification {
  id: string
  type: string
  title: string
  body: string
  is_read: boolean
  created_at: string
}

export const notificationApi = {
  list() {
    return api.get<Notification[]>('/notifications')
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
