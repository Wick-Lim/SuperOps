import { api } from './client'
import type { TokenPair, User } from '../lib/types'

export const authApi = {
  login(data: { email: string; password: string }) {
    return api.post<TokenPair>('/auth/login', data)
  },
  logout(refreshToken: string) {
    return api.post<{ message: string }>('/auth/logout', { refresh_token: refreshToken })
  },
  getMe() {
    return api.get<User>('/users/me')
  },
  acceptInvite(data: { token: string; username: string; password: string; full_name: string }) {
    return api.post<TokenPair>('/auth/accept-invite', data)
  },
  getInviteInfo(token: string) {
    return api.get<{ email: string; workspace_name: string; role: string; inviter_name: string }>(`/auth/invite/${token}`)
  },
}
