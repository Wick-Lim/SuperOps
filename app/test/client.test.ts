import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { api, fileURL, withPaging } from '../src/api/client'
import { authApi } from '../src/api/auth'
import { ApiError, ApiErrorCode, ServerErrorCode, hasErrorCode, isApiError } from '../src/lib/errors'
import { useAuthStore } from '../src/stores/authStore'
import {
  API,
  apiFailure,
  envelope,
  htmlError,
  mockFetch,
  ok,
  resetStores,
  signIn,
  type FetchMock,
} from './helpers'

const realFetch = globalThis.fetch
let net: FetchMock = { calls: [], to: () => [] }

function bearer(headers: Record<string, string>): string | undefined {
  return headers['Authorization']
}

beforeEach(() => {
  resetStores()
})

afterEach(() => {
  globalThis.fetch = realFetch
  vi.useRealTimers()
})

// ---------------------------------------------------------------------------
// The 2FA lockout regression
// ---------------------------------------------------------------------------

describe('401 handling — which ones may refresh', () => {
  it('lets a TOTP_REQUIRED 401 from /auth/login reach the caller without refreshing or logging out', async () => {
    signIn('access-1', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path === '/auth/login') {
        return apiFailure(401, ServerErrorCode.TotpRequired, 'two-factor code required')
      }
      throw new Error(`unexpected request ${req.method} ${req.path}`)
    })

    const err = await authApi
      .login({ email: 'a@b.c', password: 'pw' })
      .then(() => null)
      .catch((e: unknown) => e)

    expect(isApiError(err)).toBe(true)
    const apiErr = err as ApiError
    expect(apiErr.code).toBe('TOTP_REQUIRED')
    expect(apiErr.status).toBe(401)
    expect(apiErr.message).toBe('two-factor code required')
    // The whole point: no refresh attempt, no logout, session untouched.
    expect(net.to('/auth/refresh')).toHaveLength(0)
    expect(net.to('/auth/logout')).toHaveLength(0)
    expect(useAuthStore.getState().accessToken).toBe('access-1')
    expect(useAuthStore.getState().isAuthenticated).toBe(true)
  })

  it('keeps INVALID_TOTP_CODE intact on the second login attempt', async () => {
    net = mockFetch(() => apiFailure(401, ServerErrorCode.InvalidTotpCode, 'that code is wrong'))

    const err = await authApi
      .login({ email: 'a@b.c', password: 'pw', totp_code: '000000' })
      .then(() => null)
      .catch((e: unknown) => e)

    expect(hasErrorCode(err, ServerErrorCode.InvalidTotpCode)).toBe(true)
    expect(net.calls).toHaveLength(1)
  })

  it('never refreshes for a credential route, even on a plain 401', async () => {
    // Re-logging in while a dead session is still in the store: refreshing that
    // session cannot make wrong credentials right, and doing so would replace
    // the login screen's error with "Session expired".
    signIn('stale', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path === '/auth/login') return apiFailure(401, 'INVALID_CREDENTIALS', 'wrong password')
      throw new Error(`unexpected request ${req.path}`)
    })

    const err = await authApi
      .login({ email: 'a@b.c', password: 'nope' })
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe('INVALID_CREDENTIALS')
    expect((err as ApiError).message).toBe('wrong password')
    expect(net.to('/auth/refresh')).toHaveLength(0)
    expect(net.to('/auth/logout')).toHaveLength(0)
    expect(net.to('/auth/login')).toHaveLength(1)
  })

  it('never refreshes for the invite routes either (prefix match)', async () => {
    signIn('stale', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path.startsWith('/auth/invite/')) {
        return apiFailure(401, ServerErrorCode.Unauthorized, 'this invite link is no longer valid')
      }
      throw new Error(`unexpected request ${req.path}`)
    })

    const err = await authApi
      .getInviteInfo('tok')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).message).toBe('this invite link is no longer valid')
    expect(net.to('/auth/refresh')).toHaveLength(0)
    expect(net.to('/auth/logout')).toHaveLength(0)
  })

  it('does not refresh on a REAUTH_REQUIRED 401 from a normal authenticated route', async () => {
    signIn()
    net = mockFetch((req) => {
      if (req.path === '/auth/totp/setup') {
        return apiFailure(401, ServerErrorCode.ReauthRequired, 'confirm your password')
      }
      throw new Error(`unexpected request ${req.path}`)
    })

    const err = await authApi
      .totpSetup()
      .then(() => null)
      .catch((e: unknown) => e)

    expect(hasErrorCode(err, ServerErrorCode.ReauthRequired)).toBe(true)
    expect(net.to('/auth/refresh')).toHaveLength(0)
    expect(useAuthStore.getState().accessToken).toBe('access-1')
  })

  it('does refresh on a plain UNAUTHORIZED 401 (the session-expiry case)', async () => {
    signIn('stale', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path === '/auth/refresh') {
        return ok({ access_token: 'fresh', refresh_token: 'refresh-2' })
      }
      return bearer(req.headers) === 'Bearer fresh'
        ? ok({ id: 'u-self' })
        : apiFailure(401, ServerErrorCode.Unauthorized)
    })

    const res = await authApi.getMe()
    expect(res.data).toEqual({ id: 'u-self' })
    expect(net.to('/auth/refresh')).toHaveLength(1)
    expect(useAuthStore.getState().accessToken).toBe('fresh')
  })
})

// ---------------------------------------------------------------------------
// Single-flight refresh
// ---------------------------------------------------------------------------

describe('refresh coalescing', () => {
  it('issues exactly ONE refresh for concurrent 401s and completes both requests', async () => {
    signIn('stale', 'refresh-1')
    net = mockFetch(async (req) => {
      if (req.path === '/auth/refresh') {
        // A real refresh is not instantaneous; make the window wide enough that
        // an uncoalesced implementation would fire twice.
        await new Promise((r) => setTimeout(r, 5))
        return ok({ access_token: 'fresh', refresh_token: 'refresh-2' })
      }
      if (bearer(req.headers) === 'Bearer fresh') return ok({ path: req.path })
      return apiFailure(401, ServerErrorCode.Unauthorized)
    })

    const [a, b, c] = await Promise.all([
      api.get<{ path: string }>('/one'),
      api.get<{ path: string }>('/two'),
      api.get<{ path: string }>('/three'),
    ])

    expect(net.to('/auth/refresh')).toHaveLength(1)
    expect([a.data.path, b.data.path, c.data.path].sort()).toEqual(['/one', '/three', '/two'])
    expect(useAuthStore.getState().accessToken).toBe('fresh')
    expect(net.to('/auth/logout')).toHaveLength(0)
  })

  it('retries without refreshing when another request already rotated the token', async () => {
    signIn('stale', 'refresh-1')
    let seen = 0
    net = mockFetch((req) => {
      if (req.path !== '/late') throw new Error(`unexpected ${req.path}`)
      seen++
      if (seen === 1) {
        // Simulate a peer request finishing its refresh while this one was in flight.
        useAuthStore.setState({ accessToken: 'fresh', refreshToken: 'refresh-2' })
        return apiFailure(401, ServerErrorCode.Unauthorized)
      }
      return ok({ ok: true })
    })

    const res = await api.get<{ ok: boolean }>('/late')
    expect(res.data.ok).toBe(true)
    expect(seen).toBe(2)
    expect(net.to('/auth/refresh')).toHaveLength(0)
  })
})

// ---------------------------------------------------------------------------
// Retry depth
// ---------------------------------------------------------------------------

describe('retry depth cap', () => {
  it('does not recurse when the retried request 401s again', async () => {
    signIn('stale', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path === '/auth/refresh') {
        return ok({ access_token: 'fresh', refresh_token: 'refresh-2' })
      }
      // The server keeps rejecting even with a brand-new token.
      return apiFailure(401, ServerErrorCode.Unauthorized, 'still no')
    })

    const err = await api
      .get('/loop')
      .then(() => null)
      .catch((e: unknown) => e)

    expect(isApiError(err)).toBe(true)
    // The second 401 is surfaced verbatim, NOT turned into another refresh.
    expect((err as ApiError).code).toBe('UNAUTHORIZED')
    expect((err as ApiError).message).toBe('still no')
    expect(net.to('/loop')).toHaveLength(2)
    expect(net.to('/auth/refresh')).toHaveLength(1)
    // Nothing recursed past the cap, so no logout was triggered either.
    expect(net.to('/auth/logout')).toHaveLength(0)
  })

  it('logs out with SESSION_EXPIRED when the refresh itself fails', async () => {
    signIn('stale', 'refresh-1')
    net = mockFetch((req) => {
      if (req.path === '/auth/refresh') return apiFailure(401, 'INVALID_REFRESH_TOKEN')
      if (req.path === '/auth/logout') return ok({ message: 'ok' })
      return apiFailure(401, ServerErrorCode.Unauthorized)
    })

    const err = await api
      .get('/whatever')
      .then(() => null)
      .catch((e: unknown) => e)

    expect(isApiError(err)).toBe(true)
    expect((err as ApiError).code).toBe(ApiErrorCode.SessionExpired)
    expect((err as ApiError).isAuthExpired).toBe(true)
    expect(useAuthStore.getState().accessToken).toBeNull()
    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    // Best-effort server-side revoke still went out.
    expect(net.to('/auth/logout')).toHaveLength(1)
  })

  it('does not attempt a refresh when there is no refresh token', async () => {
    useAuthStore.setState({ accessToken: 'access-1', refreshToken: null, isAuthenticated: true })
    net = mockFetch(() => apiFailure(401, ServerErrorCode.Unauthorized, 'nope'))

    const err = await api
      .get('/thing')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe('UNAUTHORIZED')
    expect(net.to('/auth/refresh')).toHaveLength(0)
    expect(net.to('/thing')).toHaveLength(1)
  })
})

// ---------------------------------------------------------------------------
// Body parsing
// ---------------------------------------------------------------------------

describe('response parsing', () => {
  it('turns an nginx 502 HTML page into a typed ApiError, not a SyntaxError', async () => {
    signIn()
    net = mockFetch(() => htmlError(502))

    const err = await api
      .get('/channels')
      .then(() => null)
      .catch((e: unknown) => e)

    expect(isApiError(err)).toBe(true)
    expect(err).not.toBeInstanceOf(SyntaxError)
    expect((err as ApiError).status).toBe(502)
    expect((err as ApiError).code).toBe('SERVICE_UNAVAILABLE')
    expect((err as ApiError).message).not.toMatch(/JSON/i)
  })

  it('rejects a 2xx whose body is not the envelope', async () => {
    signIn()
    net = mockFetch(() => new Response('<html>hi</html>', { status: 200 }))

    const err = await api
      .get('/channels')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe(ApiErrorCode.InvalidResponse)
    expect((err as ApiError).status).toBe(200)
  })

  it('rejects an empty 200 body rather than resolving with undefined data', async () => {
    signIn()
    net = mockFetch(() => new Response('', { status: 200 }))

    const err = await api
      .get('/channels')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe(ApiErrorCode.InvalidResponse)
  })

  it('falls back to a status-derived code when an error body has none', async () => {
    signIn()
    net = mockFetch(() => envelope(429, { error: { code: '', message: '' } }))

    const err = await api
      .get('/messages')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe('RATE_LIMITED')
    expect((err as ApiError).status).toBe(429)
  })

  it('surfaces a 2xx envelope that still carries an error object', async () => {
    signIn()
    net = mockFetch(() => envelope(200, { error: { code: 'WEIRD', message: 'weird' } }))

    const err = await api
      .get('/messages')
      .then(() => null)
      .catch((e: unknown) => e)

    expect((err as ApiError).code).toBe('WEIRD')
  })

  it('normalizes list metadata through the envelope', async () => {
    signIn()
    net = mockFetch(() => ok([{ id: 'm1' }], { cursor: 'abc', has_more: true }))

    const res = await api.get<{ id: string }[]>('/channels/c1/messages')
    expect(res.data).toEqual([{ id: 'm1' }])
    expect(res.meta).toEqual({ cursor: 'abc', has_more: true })
  })
})

// ---------------------------------------------------------------------------
// Transport faults
// ---------------------------------------------------------------------------

describe('transport faults', () => {
  it('aborts at the deadline and surfaces a typed TIMEOUT error', async () => {
    vi.useFakeTimers()
    signIn()

    let aborted = false
    globalThis.fetch = vi.fn(
      (_input: unknown, init: RequestInit = {}) =>
        new Promise<Response>((_resolve, reject) => {
          init.signal?.addEventListener('abort', () => {
            aborted = true
            reject(new DOMException('The operation was aborted.', 'AbortError'))
          })
        }),
    ) as unknown as typeof fetch

    const pending = api.get('/slow').then(
      () => null,
      (e: unknown) => e,
    )
    // REQUEST_TIMEOUT_MS is 15s.
    await vi.advanceTimersByTimeAsync(15_000)
    const err = await pending

    expect(aborted).toBe(true)
    expect(isApiError(err)).toBe(true)
    expect((err as ApiError).code).toBe(ApiErrorCode.Timeout)
    expect((err as ApiError).status).toBe(0)
    expect((err as ApiError).isNetwork).toBe(true)
    expect((err as ApiError).message).toMatch(/timed out after 15s/)
  })

  it('does not fire the timeout for a request that completes in time', async () => {
    vi.useFakeTimers()
    signIn()
    net = mockFetch(() => ok({ id: 'u-self' }))

    const res = await api.get<{ id: string }>('/users/me')
    expect(res.data.id).toBe('u-self')
    // No pending abort timer was left behind.
    expect(vi.getTimerCount()).toBe(0)
  })

  it('maps a transport failure to NETWORK_ERROR', async () => {
    signIn()
    globalThis.fetch = vi.fn(() => Promise.reject(new TypeError('Failed to fetch'))) as unknown as typeof fetch

    const err = await api
      .get('/users/me')
      .then(() => null)
      .catch((e: unknown) => e)

    expect(isApiError(err)).toBe(true)
    expect((err as ApiError).code).toBe(ApiErrorCode.Network)
    expect((err as ApiError).isNetwork).toBe(true)
  })
})

// ---------------------------------------------------------------------------
// Request shaping
// ---------------------------------------------------------------------------

describe('request shaping', () => {
  it('sends the bearer token and omits Content-Type on a bodiless request', async () => {
    signIn('tok-9')
    net = mockFetch(() => ok({ ok: true }))

    await api.get('/users/me')
    await api.post('/channels/c1/join')
    await api.post('/channels/c1/messages', { content: 'hi' })

    expect(net.calls[0].headers['Authorization']).toBe('Bearer tok-9')
    expect(net.calls[0].headers['Content-Type']).toBeUndefined()
    // POST with no body is still bodiless — no content type.
    expect(net.calls[1].headers['Content-Type']).toBeUndefined()
    expect(net.calls[2].headers['Content-Type']).toBe('application/json')
    expect(net.calls[2].body).toEqual({ content: 'hi' })
  })

  it('sends no Authorization header when signed out', async () => {
    net = mockFetch(() => ok({ ok: true }))
    await api.get('/auth/invite/abc')
    expect(net.calls[0].headers['Authorization']).toBeUndefined()
  })

  it('refreshes once for an upload 401 and retries the same FormData', async () => {
    signIn('stale', 'refresh-1')
    const form = new FormData()
    form.append('name', 'x.png')

    net = mockFetch((req) => {
      if (req.path === '/auth/refresh') return ok({ access_token: 'fresh', refresh_token: 'refresh-2' })
      return bearer(req.headers) === 'Bearer fresh'
        ? ok({ id: 'f1' })
        : apiFailure(401, ServerErrorCode.Unauthorized)
    })

    const res = await api.upload<{ id: string }>('/files', form)
    expect(res.data.id).toBe('f1')
    expect(net.to('/auth/refresh')).toHaveLength(1)
    expect(net.to('/files')).toHaveLength(2)
    // Multipart boundary is the runtime's job — the client must not set one.
    expect(net.to('/files')[0].headers['Content-Type']).toBeUndefined()
    expect(net.to('/files')[1].body).toBe(form)
  })
})

// ---------------------------------------------------------------------------
// URL helpers
// ---------------------------------------------------------------------------

describe('url helpers', () => {
  it('encodes the opaque cursor and picks the right separator', () => {
    expect(withPaging('/notifications')).toBe('/notifications')
    expect(withPaging('/notifications', undefined, 50)).toBe('/notifications?limit=50')
    expect(withPaging('/notifications', 'a+b/c==', 50)).toBe('/notifications?cursor=a%2Bb%2Fc%3D%3D&limit=50')
    expect(withPaging('/search?q=hi', 'cur')).toBe('/search?q=hi&cursor=cur')
  })

  it('appends the token to a file URL only while signed in', () => {
    expect(fileURL('f 1')).toBe(`${API}/files/f%201`)
    signIn('tok/9')
    expect(fileURL('f1')).toBe(`${API}/files/f1?token=tok%2F9`)
  })
})
