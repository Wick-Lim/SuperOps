import { api } from './client'
import type { User, PublicUser } from '../lib/types'

export const userApi = {
  getMe() {
    return api.get<User>('/users/me')
  },
  updateMe(data: { full_name?: string; avatar_url?: string; timezone?: string; locale?: string }) {
    return api.patch<User>('/users/me', data)
  },
  updateStatus(data: { status_text: string; status_emoji: string }) {
    return api.put<{ status_text: string; status_emoji: string }>('/users/me/status', data)
  },
  get(userId: string) {
    return api.get<PublicUser>(`/users/${userId}`)
  },
  search(query: string) {
    return api.get<PublicUser[]>(`/users/search?q=${encodeURIComponent(query)}`)
  },
}
