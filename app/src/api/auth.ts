import { api } from './client'
import type { TokenPair, User } from '../lib/types'

export const authApi = {
  /**
   * Answers 401 `TOTP_REQUIRED` when the password is correct but the account
   * has 2FA on — retry with `totp_code`. That code now reaches the caller: the
   * client no longer intercepts every 401 as a session expiry.
   */
  login(data: { email: string; password: string; totp_code?: string }) {
    return api.post<TokenPair>('/auth/login', data)
  },
  /** Revokes the server-side session row. Called by `useAuthStore.logout()`. */
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
    return api.get<{ email: string; workspace_name: string; role: string; inviter_name: string }>(
      `/auth/invite/${encodeURIComponent(token)}`,
    )
  },
  changePassword(data: { old_password: string; new_password: string }) {
    return api.post<{ message: string }>('/auth/change-password', data)
  },
  totpStatus() {
    return api.get<{ enabled: boolean }>('/auth/totp/status')
  },
  /**
   * First-time enrolment needs no arguments. RE-enrolling while 2FA is already
   * on requires proof of possession — pass the account password or a current
   * code, or the call answers 401 `REAUTH_REQUIRED`.
   */
  totpSetup(proof?: { password?: string; code?: string }) {
    return api.post<{ secret: string; otpauth_url: string }>('/auth/totp/setup', {
      password: proof?.password,
      code: proof?.code,
    })
  },
  totpVerify(code: string) {
    return api.post<{ enabled: boolean; backup_codes: string[] }>('/auth/totp/verify', { code })
  },
  totpDisable(code: string) {
    return api.post<{ enabled: boolean }>('/auth/totp/disable', { code })
  },
}
