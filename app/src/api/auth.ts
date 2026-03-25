import { api } from './client'
import type { TokenPair, User } from '../lib/types'

export const authApi = {
  register(data: { email: string; username: string; password: string; full_name: string }) {
    return api.post<{ id: string; email: string; username: string }>('/auth/register', data)
  },
  login(data: { email: string; password: string }) {
    return api.post<TokenPair>('/auth/login', data)
  },
  logout(refreshToken: string) {
    return api.post<{ message: string }>('/auth/logout', { refresh_token: refreshToken })
  },
  getMe() {
    return api.get<User>('/users/me')
  },
}
