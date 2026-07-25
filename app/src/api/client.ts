import type { ApiResponse } from '../lib/types'
import { ApiError, ApiErrorCode, statusFallback } from '../lib/errors'
import { useAuthStore } from '../stores/authStore'
import { API_BASE_URL, REQUEST_TIMEOUT_MS, UPLOAD_TIMEOUT_MS } from '../config'

export { ApiError, ApiErrorCode, ServerErrorCode, isApiError, hasErrorCode, errorMessage } from '../lib/errors'

/**
 * Auth routes that authenticate the *caller* rather than a session. A 401 from
 * one of these means "those credentials are wrong" (or "send a TOTP code"), so
 * refreshing the access token cannot help and would destroy the login screen's
 * error: the old client turned every 401 into `Error('Session expired')`, which
 * made 2FA login literally unreachable.
 */
const CREDENTIAL_PATHS = ['/auth/login', '/auth/refresh', '/auth/logout', '/auth/accept-invite', '/auth/invite/']

/**
 * 401 codes a fresh access token cannot fix. They describe the *request*, not
 * the session, and must reach the caller verbatim.
 */
const NON_SESSION_401 = new Set([
  'TOTP_REQUIRED',
  'INVALID_TOTP_CODE',
  'TOTP_SETUP_REQUIRED',
  'REAUTH_REQUIRED',
  'INVITE_WRONG_ACCOUNT',
])

function isCredentialPath(path: string): boolean {
  return CREDENTIAL_PATHS.some((p) => path.startsWith(p))
}

interface Envelope {
  data?: unknown
  meta?: { cursor?: string; has_more: boolean }
  error?: { code: string; message: string; details?: unknown }
}

/**
 * Reads the body as text first, then parses. `res.json()` on an nginx 502 HTML
 * page or an empty 204 body throws a raw SyntaxError that reaches the user as
 * "JSON Parse error"; here a non-envelope body simply yields undefined and the
 * status decides the error.
 */
async function readEnvelope(res: Response): Promise<Envelope | undefined> {
  let text: string
  try {
    text = await res.text()
  } catch {
    return undefined
  }
  if (!text) return undefined
  try {
    const parsed = JSON.parse(text)
    return parsed && typeof parsed === 'object' ? (parsed as Envelope) : undefined
  } catch {
    return undefined
  }
}

function errorFrom(status: number, body: Envelope | undefined): ApiError {
  if (body?.error) {
    const fallback = statusFallback(status)
    return new ApiError(
      status,
      body.error.code || fallback.code,
      body.error.message || fallback.message,
      body.error.details,
    )
  }
  const { code, message } = statusFallback(status)
  return new ApiError(status, code, message)
}

/** Wraps fetch with an AbortController deadline and maps transport faults to ApiError. */
async function fetchWithTimeout(url: string, init: RequestInit, timeoutMs: number): Promise<Response> {
  const controller = new AbortController()
  const timer = setTimeout(() => controller.abort(), timeoutMs)
  try {
    return await fetch(url, { ...init, signal: controller.signal })
  } catch (err) {
    if (controller.signal.aborted) {
      throw new ApiError(0, ApiErrorCode.Timeout, `The request timed out after ${Math.round(timeoutMs / 1000)}s.`)
    }
    throw new ApiError(0, ApiErrorCode.Network, 'Could not reach the server. Check your connection.')
  } finally {
    clearTimeout(timer)
  }
}

class ApiClient {
  /**
   * The single in-flight refresh. RefreshTokens rotates by DELETING the session
   * row before issuing (backend/internal/auth/service.go), so two concurrent
   * refreshes mean the second one 401s and logs the user out. WorkspaceScreen
   * fires three parallel requests on mount, which made that reachable on every
   * session resume.
   */
  private refreshInFlight: Promise<boolean> | null = null

  private getHeaders(hasBody: boolean): Record<string, string> {
    const headers: Record<string, string> = {}
    // Only declare a content type when there is content: a GET/DELETE with
    // `Content-Type: application/json` and no body is a malformed request.
    if (hasBody) headers['Content-Type'] = 'application/json'
    const token = useAuthStore.getState().accessToken
    if (token) headers['Authorization'] = `Bearer ${token}`
    return headers
  }

  async request<T>(method: string, path: string, body?: unknown, retriesLeft = 1): Promise<ApiResponse<T>> {
    const hasBody = body !== undefined && body !== null
    // Captured before the call so a 401 can tell "my token is stale" from
    // "another request already refreshed while this one was in flight".
    const tokenAtSend = useAuthStore.getState().accessToken

    const res = await fetchWithTimeout(
      `${API_BASE_URL}${path}`,
      {
        method,
        headers: this.getHeaders(hasBody),
        body: hasBody ? JSON.stringify(body) : undefined,
      },
      REQUEST_TIMEOUT_MS,
    )

    const envelope = await readEnvelope(res)

    if (res.status === 401 && retriesLeft > 0 && !isCredentialPath(path)) {
      const code = envelope?.error?.code
      if (!code || !NON_SESSION_401.has(code)) {
        // Someone else already rotated the token — just retry with the new one.
        if (useAuthStore.getState().accessToken !== tokenAtSend) {
          return this.request<T>(method, path, body, retriesLeft - 1)
        }
        if (useAuthStore.getState().refreshToken) {
          if (await this.tryRefresh()) {
            return this.request<T>(method, path, body, retriesLeft - 1)
          }
          await useAuthStore.getState().logout()
          throw new ApiError(401, ApiErrorCode.SessionExpired, 'Your session expired. Please sign in again.')
        }
      }
    }

    if (!res.ok) throw errorFrom(res.status, envelope)
    if (!envelope) {
      throw new ApiError(
        res.status,
        ApiErrorCode.InvalidResponse,
        'The server returned an unreadable response.',
      )
    }
    // A 2xx with an error body should not happen, but the envelope allows it.
    if (envelope.error) throw errorFrom(res.status, envelope)

    return envelope as ApiResponse<T>
  }

  /** Coalesces concurrent refreshes onto one request. */
  private tryRefresh(): Promise<boolean> {
    if (this.refreshInFlight) return this.refreshInFlight
    const inFlight = this.doRefresh().then(
      (ok) => {
        this.refreshInFlight = null
        return ok
      },
      () => {
        this.refreshInFlight = null
        return false
      },
    )
    this.refreshInFlight = inFlight
    return inFlight
  }

  private async doRefresh(): Promise<boolean> {
    const refreshToken = useAuthStore.getState().refreshToken
    if (!refreshToken) return false
    try {
      const res = await fetchWithTimeout(
        `${API_BASE_URL}/auth/refresh`,
        {
          method: 'POST',
          headers: { 'Content-Type': 'application/json' },
          body: JSON.stringify({ refresh_token: refreshToken }),
        },
        REQUEST_TIMEOUT_MS,
      )
      if (!res.ok) return false
      const envelope = await readEnvelope(res)
      const pair = envelope?.data as { access_token?: string; refresh_token?: string } | undefined
      if (!pair?.access_token || !pair.refresh_token) return false
      await useAuthStore.getState().setTokens(pair.access_token, pair.refresh_token)
      return true
    } catch {
      return false
    }
  }

  /**
   * Multipart upload (FormData) — used for file attachments. Do not set
   * Content-Type; the runtime sets the multipart boundary automatically.
   */
  async upload<T>(path: string, form: FormData, retriesLeft = 1): Promise<ApiResponse<T>> {
    const tokenAtSend = useAuthStore.getState().accessToken
    const headers: Record<string, string> = {}
    if (tokenAtSend) headers['Authorization'] = `Bearer ${tokenAtSend}`

    const res = await fetchWithTimeout(
      `${API_BASE_URL}${path}`,
      { method: 'POST', headers, body: form },
      UPLOAD_TIMEOUT_MS,
    )
    const envelope = await readEnvelope(res)

    if (res.status === 401 && retriesLeft > 0) {
      if (useAuthStore.getState().accessToken !== tokenAtSend) {
        return this.upload<T>(path, form, retriesLeft - 1)
      }
      if (useAuthStore.getState().refreshToken && (await this.tryRefresh())) {
        return this.upload<T>(path, form, retriesLeft - 1)
      }
      await useAuthStore.getState().logout()
      throw new ApiError(401, ApiErrorCode.SessionExpired, 'Your session expired. Please sign in again.')
    }

    if (!res.ok) throw errorFrom(res.status, envelope)
    if (!envelope) {
      throw new ApiError(res.status, ApiErrorCode.InvalidResponse, 'The server returned an unreadable response.')
    }
    if (envelope.error) throw errorFrom(res.status, envelope)
    return envelope as ApiResponse<T>
  }

  get<T>(path: string) { return this.request<T>('GET', path) }
  post<T>(path: string, body?: unknown) { return this.request<T>('POST', path, body) }
  patch<T>(path: string, body?: unknown) { return this.request<T>('PATCH', path, body) }
  put<T>(path: string, body?: unknown) { return this.request<T>('PUT', path, body) }
  del<T>(path: string, body?: unknown) { return this.request<T>('DELETE', path, body) }
}

export const api = new ApiClient()

/**
 * Absolute URL for a stored file (for `<Image>`/download); appends the bearer
 * token as a query param since RN's `<Image>` cannot set headers.
 *
 * `?token=` is accepted on exactly two routes — `GET /api/v1/ws` and
 * `GET /api/v1/files/{id}` (backend/internal/auth/middleware.go
 * `allowsQueryToken`). Everything else must use the Authorization header; do
 * not copy this pattern.
 */
export function fileURL(fileId: string): string {
  const token = useAuthStore.getState().accessToken
  return `${API_BASE_URL}/files/${encodeURIComponent(fileId)}${token ? `?token=${encodeURIComponent(token)}` : ''}`
}

/** Appends `cursor`/`limit` to a path, encoding the opaque base64 cursor. */
export function withPaging(path: string, cursor?: string, limit?: number): string {
  const parts: string[] = []
  if (cursor) parts.push(`cursor=${encodeURIComponent(cursor)}`)
  if (limit) parts.push(`limit=${limit}`)
  if (parts.length === 0) return path
  return `${path}${path.includes('?') ? '&' : '?'}${parts.join('&')}`
}
