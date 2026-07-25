import { api } from './client'
import type { User, PublicUser } from '../lib/types'

export const userApi = {
  getMe() {
    return api.get<User>('/users/me')
  },
  updateMe(data: { full_name?: string; avatar_url?: string; timezone?: string; locale?: string }) {
    return api.patch<User>('/users/me', data)
  },
  /**
   * Answers only the two fields it wrote, not a User. They do round-trip now:
   * GET /users/me and GET /users/{id} both return status_text/status_emoji.
   */
  updateStatus(data: { status_text: string; status_emoji: string }) {
    return api.put<{ status_text: string; status_emoji: string }>('/users/me/status', data)
  },
  get(userId: string) {
    return api.get<PublicUser>(`/users/${userId}`)
  },
  /** Username / full-name prefix search, scoped to workspaces shared with the caller. */
  search(query: string) {
    return api.get<PublicUser[]>(`/users/search?q=${encodeURIComponent(query)}`)
  },
}
