import { api } from './client'
import type { TokenPair, User } from '../lib/types'

export const authApi = {
  login(data: { email: string; password: string; totp_code?: string }) {
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
  changePassword(data: { old_password: string; new_password: string }) {
    return api.post<{ message: string }>('/auth/change-password', data)
  },
  totpStatus() {
    return api.get<{ enabled: boolean }>('/auth/totp/status')
  },
  totpSetup() {
    return api.post<{ secret: string; otpauth_url: string }>('/auth/totp/setup')
  },
  totpVerify(code: string) {
    return api.post<{ enabled: boolean; backup_codes: string[] }>('/auth/totp/verify', { code })
  },
  totpDisable(code: string) {
    return api.post<{ enabled: boolean }>('/auth/totp/disable', { code })
  },
}
