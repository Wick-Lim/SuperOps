import type { ApiResponse } from '../lib/types'
import { useAuthStore } from '../stores/authStore'
import { API_BASE_URL } from '../config'

class ApiClient {
  private getHeaders(): Record<string, string> {
    const headers: Record<string, string> = { 'Content-Type': 'application/json' }
    const token = useAuthStore.getState().accessToken
    if (token) headers['Authorization'] = `Bearer ${token}`
    return headers
  }

  async request<T>(method: string, path: string, body?: unknown): Promise<ApiResponse<T>> {
    const res = await fetch(`${API_BASE_URL}${path}`, {
      method,
      headers: this.getHeaders(),
      body: body ? JSON.stringify(body) : undefined,
    })

    if (res.status === 401) {
      const refreshed = await this.tryRefresh()
      if (refreshed) return this.request<T>(method, path, body)
      useAuthStore.getState().logout()
      throw new Error('Session expired')
    }

    const data = await res.json()
    if (data.error) throw new Error(data.error.message)
    return data
  }

  private async tryRefresh(): Promise<boolean> {
    const refreshToken = useAuthStore.getState().refreshToken
    if (!refreshToken) return false
    try {
      const res = await fetch(`${API_BASE_URL}/auth/refresh`, {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      })
      if (!res.ok) return false
      const data = await res.json()
      if (data.data) {
        useAuthStore.getState().setTokens(data.data.access_token, data.data.refresh_token)
        return true
      }
      return false
    } catch { return false }
  }

  // Multipart upload (FormData) — used for file attachments. Do not set
  // Content-Type; the runtime sets the multipart boundary automatically.
  async upload<T>(path: string, form: FormData): Promise<ApiResponse<T>> {
    const token = useAuthStore.getState().accessToken
    const headers: Record<string, string> = {}
    if (token) headers['Authorization'] = `Bearer ${token}`

    let res = await fetch(`${API_BASE_URL}${path}`, { method: 'POST', headers, body: form })
    if (res.status === 401 && (await this.tryRefresh())) {
      const t2 = useAuthStore.getState().accessToken
      res = await fetch(`${API_BASE_URL}${path}`, {
        method: 'POST',
        headers: t2 ? { Authorization: `Bearer ${t2}` } : {},
        body: form,
      })
    }
    const data = await res.json()
    if (data.error) throw new Error(data.error.message)
    return data
  }

  get<T>(path: string) { return this.request<T>('GET', path) }
  post<T>(path: string, body?: unknown) { return this.request<T>('POST', path, body) }
  patch<T>(path: string, body?: unknown) { return this.request<T>('PATCH', path, body) }
  put<T>(path: string, body?: unknown) { return this.request<T>('PUT', path, body) }
  del<T>(path: string) { return this.request<T>('DELETE', path) }
}

export const api = new ApiClient()

// Absolute URL for a stored file (for <Image>/download); appends the bearer
// token as a query param since RN's <Image> cannot set headers.
export function fileURL(fileId: string): string {
  const token = useAuthStore.getState().accessToken
  return `${API_BASE_URL}/files/${fileId}${token ? `?token=${token}` : ''}`
}
