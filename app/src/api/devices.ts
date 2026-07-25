import { api } from './client'
import { API_BASE_URL, REQUEST_TIMEOUT_MS } from '../config'

export type DevicePlatform = 'ios' | 'android' | 'web'

/**
 * Push-token registration. These routes only exist when the backend runs with
 * `PUSH_ENABLED=true` (see backend/internal/app/app.go); on a deployment
 * without push they answer 404, which `lib/push.ts` treats as "no push here"
 * rather than an error worth showing anyone.
 */
export const deviceApi = {
  register(token: string, platform: DevicePlatform) {
    return api.post<{ token: string; platform: string }>('/users/me/devices', {
      token,
      platform,
    })
  },

  /**
   * Deregisters a device, deliberately bypassing the shared `api` client.
   *
   * Two reasons. First, logout is the only caller, and by the time the shared
   * client would run, the store has to be cleared — so the bearer token is
   * passed in explicitly rather than read from a store that is mid-teardown.
   * Second, `ApiClient.request` reacts to a 401 by refreshing and, if that
   * fails, calling `authStore.logout()` — which is what called us. A bare fetch
   * has no such reentrancy.
   *
   * Failure is silent by design: a device that could not be deregistered is
   * corrected the moment somebody else registers the same token, because
   * registration reassigns ownership (migration 012).
   */
  async deregister(token: string, accessToken: string | null): Promise<boolean> {
    if (!accessToken) return false

    const controller = new AbortController()
    const timer = setTimeout(() => controller.abort(), REQUEST_TIMEOUT_MS)
    try {
      const res = await fetch(`${API_BASE_URL}/users/me/devices/${encodeURIComponent(token)}`, {
        method: 'DELETE',
        headers: { Authorization: `Bearer ${accessToken}` },
        signal: controller.signal,
      })
      // 404 means it was already gone (or push is disabled server-side), which
      // is the outcome we wanted either way.
      return res.ok || res.status === 404
    } catch {
      return false
    } finally {
      clearTimeout(timer)
    }
  },
}
