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

/**
 * Turns a response and its parsed envelope into a result or an error.
 *
 * ONE COPY, because there were two and they had to agree. `request` and
 * `upload` each carried their own version of this ladder; a rule fixed in one
 * would silently not hold in the other, which is the shape of every quiet
 * regression in this codebase.
 *
 * 204 IS A SUCCESS WITH NOTHING TO SAY. Nine handlers write a bare 204 — trash
 * a file or folder, unshare, revoke a link, delete a comment, archive a
 * workflow, end a huddle, revoke an ingest token — and every one of them was
 * reported to the user as "the server returned an unreadable response". The
 * operation had already completed, so the user saw "Could not delete it" about
 * a thing that was deleted, and each success path was skipped with it: the
 * huddle bar kept showing an ended call, a deleted workflow stayed on screen,
 * a share list never reloaded.
 *
 * readEnvelope's own comment said the status decides the error. It never got
 * to; this is where the status decides.
 */
function settle<T>(res: Response, envelope: Envelope | undefined): ApiResponse<T> {
  if (!res.ok) throw errorFrom(res.status, envelope)
  if (!envelope) {
    if (res.status === 204 || res.status === 205) {
      return { data: undefined as T }
    }
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

class ApiClient {
  private sessionEpoch = 0

  /**
   * The single in-flight refresh. RefreshTokens rotates by DELETING the session
   * row before issuing (backend/internal/auth/service.go), so two concurrent
   * refreshes mean the second one 401s and logs the user out. WorkspaceScreen
   * fires three parallel requests on mount, which made that reachable on every
   * session resume.
   */
  private refreshInFlight: Promise<boolean> | null = null

  resetSession(): void {
    this.sessionEpoch += 1
    this.refreshInFlight = null
  }

  private assertCurrentSession(epoch: number): void {
    if (epoch !== this.sessionEpoch) {
      throw new ApiError(0, ApiErrorCode.SessionChanged, 'This request belonged to a previous sign-in.')
    }
  }

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
    const epoch = this.sessionEpoch
    const hasBody = body !== undefined && body !== null
    // Captured before the call so a 401 can tell "my token is stale" from
    // "another request already refreshed while this one was in flight".
    const tokenAtSend = useAuthStore.getState().accessToken
    const refreshTokenAtSend = useAuthStore.getState().refreshToken

    const res = await fetchWithTimeout(
      `${API_BASE_URL}${path}`,
      {
        method,
        headers: this.getHeaders(hasBody),
        body: hasBody ? JSON.stringify(body) : undefined,
      },
      REQUEST_TIMEOUT_MS,
    )
    this.assertCurrentSession(epoch)

    const envelope = await readEnvelope(res)
    this.assertCurrentSession(epoch)

    if (res.status === 401 && retriesLeft > 0 && !isCredentialPath(path)) {
      const code = envelope?.error?.code
      if (!code || !NON_SESSION_401.has(code)) {
        // Someone else already rotated the token — just retry with the new one.
        if (useAuthStore.getState().accessToken !== tokenAtSend) {
          this.assertCurrentSession(epoch)
          return this.request<T>(method, path, body, retriesLeft - 1)
        }
        if (useAuthStore.getState().refreshToken) {
          if (await this.tryRefresh(epoch)) {
            this.assertCurrentSession(epoch)
            return this.request<T>(method, path, body, retriesLeft - 1)
          }
          this.assertCurrentSession(epoch)
          await useAuthStore.getState().logout({
            accessToken: tokenAtSend,
            refreshToken: refreshTokenAtSend,
          })
          this.assertCurrentSession(epoch)
          throw new ApiError(401, ApiErrorCode.SessionExpired, 'Your session expired. Please sign in again.')
        }
      }
    }

    this.assertCurrentSession(epoch)
    return settle<T>(res, envelope)
  }

  /** Coalesces concurrent refreshes onto one request. */
  private tryRefresh(epoch: number): Promise<boolean> {
    this.assertCurrentSession(epoch)
    if (this.refreshInFlight) return this.refreshInFlight
    const refreshToken = useAuthStore.getState().refreshToken
    if (!refreshToken) return Promise.resolve(false)
    const inFlight = this.doRefresh(epoch, refreshToken).then(
      (ok) => {
        if (this.refreshInFlight === inFlight) this.refreshInFlight = null
        return ok
      },
      () => {
        if (this.refreshInFlight === inFlight) this.refreshInFlight = null
        return false
      },
    )
    this.refreshInFlight = inFlight
    return inFlight
  }

  private async doRefresh(epoch: number, refreshToken: string): Promise<boolean> {
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
      this.assertCurrentSession(epoch)
      if (!res.ok) return false
      const envelope = await readEnvelope(res)
      this.assertCurrentSession(epoch)
      const pair = envelope?.data as { access_token?: string; refresh_token?: string } | undefined
      if (!pair?.access_token || !pair.refresh_token) return false
      if (useAuthStore.getState().refreshToken !== refreshToken) return false
      await useAuthStore.getState().setTokens(pair.access_token, pair.refresh_token)
      this.assertCurrentSession(epoch)
      return true
    } catch (error) {
      if (error instanceof ApiError && error.code === ApiErrorCode.SessionChanged) throw error
      return false
    }
  }

  /**
   * Multipart upload (FormData) — used for file attachments. Do not set
   * Content-Type; the runtime sets the multipart boundary automatically.
   */
  async upload<T>(path: string, form: FormData, retriesLeft = 1): Promise<ApiResponse<T>> {
    const epoch = this.sessionEpoch
    const tokenAtSend = useAuthStore.getState().accessToken
    const refreshTokenAtSend = useAuthStore.getState().refreshToken
    const headers: Record<string, string> = {}
    if (tokenAtSend) headers['Authorization'] = `Bearer ${tokenAtSend}`

    const res = await fetchWithTimeout(
      `${API_BASE_URL}${path}`,
      { method: 'POST', headers, body: form },
      UPLOAD_TIMEOUT_MS,
    )
    this.assertCurrentSession(epoch)
    const envelope = await readEnvelope(res)
    this.assertCurrentSession(epoch)

    if (res.status === 401 && retriesLeft > 0) {
      if (useAuthStore.getState().accessToken !== tokenAtSend) {
        this.assertCurrentSession(epoch)
        return this.upload<T>(path, form, retriesLeft - 1)
      }
      if (useAuthStore.getState().refreshToken && (await this.tryRefresh(epoch))) {
        this.assertCurrentSession(epoch)
        return this.upload<T>(path, form, retriesLeft - 1)
      }
      this.assertCurrentSession(epoch)
      await useAuthStore.getState().logout({
        accessToken: tokenAtSend,
        refreshToken: refreshTokenAtSend,
      })
      this.assertCurrentSession(epoch)
      throw new ApiError(401, ApiErrorCode.SessionExpired, 'Your session expired. Please sign in again.')
    }

    this.assertCurrentSession(epoch)
    return settle<T>(res, envelope)
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
/**
 * Follows a cursor to the end and returns everything.
 *
 * SEVERAL SCREENS READ ONE PAGE AND CALLED IT THE TOTAL. The server's default is
 * 50; the shared inbox stopped at 50 conversations, a comment thread's header
 * printed its page length as "N comments", and a workflow's run history simply
 * ended. None read `meta`, and nothing said anything was missing.
 *
 * Bounded on purpose. A runaway cursor — a server that always answers
 * `has_more` — is worse than a truncated list, so the walk stops and SAYS it
 * stopped. A caller that ignores `truncated` is back where it started, which is
 * why it is a field and not a console warning.
 */
export async function collectPages<T>(
  fetchPage: (cursor?: string) => Promise<ApiResponse<T[]>>,
  maxPages = 20,
): Promise<{ items: T[]; truncated: boolean }> {
  const items: T[] = []
  let cursor: string | undefined
  for (let page = 0; page < maxPages; page++) {
    const res = await fetchPage(cursor)
    items.push(...(res.data ?? []))
    if (!res.meta?.has_more || !res.meta.cursor) {
      return { items, truncated: false }
    }
    cursor = res.meta.cursor
  }
  return { items, truncated: true }
}

export function withPaging(path: string, cursor?: string, limit?: number): string {
  const parts: string[] = []
  if (cursor) parts.push(`cursor=${encodeURIComponent(cursor)}`)
  if (limit) parts.push(`limit=${limit}`)
  if (parts.length === 0) return path
  return `${path}${path.includes('?') ? '&' : '?'}${parts.join('&')}`
}
