/**
 * Typed transport error.
 *
 * Every failure that leaves `ApiClient` is an ApiError, so a caller can branch
 * on `code` (the server's `error.code`, or a synthetic one for client-side
 * failures) instead of pattern-matching an English message. The old client threw
 * bare `Error`s, which is why login had to test `/two-factor/i` against a string
 * that could never contain it.
 */
export class ApiError extends Error {
  /** HTTP status, or 0 when the request never produced a response. */
  readonly status: number
  /** Server `error.code`, or one of the synthetic codes below. */
  readonly code: string
  readonly details?: unknown

  constructor(status: number, code: string, message: string, details?: unknown) {
    super(message)
    this.name = 'ApiError'
    this.status = status
    this.code = code
    this.details = details
    // Required for `instanceof` to survive the ES5 target's class downlevelling.
    Object.setPrototypeOf(this, ApiError.prototype)
  }

  /** The request never reached the server (offline, DNS, TLS, timeout). */
  get isNetwork(): boolean {
    return this.status === 0
  }

  /** The session is gone; the caller should route to Login rather than retry. */
  get isAuthExpired(): boolean {
    return this.code === ApiErrorCode.SessionExpired
  }
}

/** Synthetic codes produced by the client itself (never sent by the server). */
export const ApiErrorCode = {
  Network: 'NETWORK_ERROR',
  Timeout: 'TIMEOUT',
  /** Response was not the `{data, meta, error}` envelope (proxy error page, empty body). */
  InvalidResponse: 'INVALID_RESPONSE',
  /** Refresh failed or was impossible; the local session has been cleared. */
  SessionExpired: 'SESSION_EXPIRED',
  Unknown: 'UNKNOWN',
} as const

/** Server codes the UI is expected to branch on. */
export const ServerErrorCode = {
  TotpRequired: 'TOTP_REQUIRED',
  InvalidTotpCode: 'INVALID_TOTP_CODE',
  TotpSetupRequired: 'TOTP_SETUP_REQUIRED',
  ReauthRequired: 'REAUTH_REQUIRED',
  Unauthorized: 'UNAUTHORIZED',
  Forbidden: 'FORBIDDEN',
  NotFound: 'NOT_FOUND',
  Conflict: 'CONFLICT',
  BadRequest: 'BAD_REQUEST',
  RateLimited: 'RATE_LIMITED',
  ChannelArchived: 'CHANNEL_ARCHIVED',
  Blocked: 'BLOCKED',
  UseOwnershipTransfer: 'USE_OWNERSHIP_TRANSFER',
} as const

export function isApiError(err: unknown): err is ApiError {
  return err instanceof ApiError
}

/** True when `err` is an ApiError carrying any of `codes`. */
export function hasErrorCode(err: unknown, ...codes: string[]): boolean {
  return isApiError(err) && codes.includes(err.code)
}

/** Message suitable for `Alert.alert('Error', ...)` regardless of the thrown value. */
export function errorMessage(err: unknown, fallback = 'Something went wrong'): string {
  if (isApiError(err)) return err.message
  if (err instanceof Error && err.message) return err.message
  return fallback
}

/** Fallback code/message for a status with no parsable envelope. */
export function statusFallback(status: number): { code: string; message: string } {
  switch (status) {
    case 400:
      return { code: 'BAD_REQUEST', message: 'The request was rejected.' }
    case 401:
      return { code: 'UNAUTHORIZED', message: 'You are not signed in.' }
    case 403:
      return { code: 'FORBIDDEN', message: 'You do not have access to this.' }
    case 404:
      return { code: 'NOT_FOUND', message: 'Not found.' }
    case 409:
      return { code: 'CONFLICT', message: 'That conflicts with the current state.' }
    case 413:
      return { code: 'PAYLOAD_TOO_LARGE', message: 'That upload is too large.' }
    case 429:
      return { code: 'RATE_LIMITED', message: 'Too many requests — try again shortly.' }
    case 502:
    case 503:
    case 504:
      return { code: 'SERVICE_UNAVAILABLE', message: 'The server is unavailable. Try again shortly.' }
    default:
      if (status >= 500) return { code: 'INTERNAL_ERROR', message: 'The server hit an unexpected error.' }
      return { code: ApiErrorCode.Unknown, message: `Request failed (HTTP ${status}).` }
  }
}
