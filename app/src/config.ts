import { NativeModules, Platform } from 'react-native'

// Every API route lives under this prefix (backend/internal/*/handler.go).
const API_PREFIX = '/api/v1'
// `make dev` runs the API directly on SERVER_PORT (default 8080). `make
// docker-up` publishes it on 8081. Override with EXPO_PUBLIC_API_PORT.
const DEFAULT_DEV_PORT = '8080'

// @ts-ignore __DEV__ is injected by the RN/Expo runtime
const isDev: boolean = typeof __DEV__ !== 'undefined' ? __DEV__ : true

// EXPO_PUBLIC_* values are inlined at build time; process.env itself is absent
// in some runtimes, hence the guard.
const env: Record<string, string | undefined> =
  typeof process !== 'undefined' && process.env ? (process.env as Record<string, string | undefined>) : {}

const webLocation: Location | undefined =
  typeof window !== 'undefined' && typeof window.location !== 'undefined' ? window.location : undefined

function stripTrailingSlash(url: string): string {
  return url.replace(/\/+$/, '')
}

/**
 * Host the JS bundle was served from. On a simulator this is `localhost`; on a
 * physical device or an emulator it is the dev machine's LAN address — the only
 * address that can reach the API. Reading it is why a device build works at all:
 * a hardcoded `localhost` resolves to the device itself.
 */
function metroHost(): string | null {
  const scriptURL: unknown = (NativeModules as Record<string, any>)?.SourceCode?.scriptURL
  if (typeof scriptURL !== 'string') return null
  const match = /^[a-z][a-z0-9+.-]*:\/\/([^/:?#]+)/i.exec(scriptURL)
  return match ? match[1] : null
}

function devHost(): string {
  if (webLocation) return webLocation.hostname || 'localhost'
  const host = metroHost()
  if (Platform.OS === 'android') {
    // The Android emulator's own loopback is the emulator, not the host; 10.0.2.2
    // is the documented alias for the host machine. A LAN address from Metro
    // (physical device, or `adb reverse` not set up) is preferred when present.
    if (host && host !== 'localhost' && host !== '127.0.0.1') return host
    return '10.0.2.2'
  }
  return host || 'localhost'
}

function resolveApiBase(): string {
  // An explicit URL always wins — this is the only knob a release build has.
  const fromEnv = env.EXPO_PUBLIC_API_URL
  if (fromEnv) return stripTrailingSlash(fromEnv)

  if (isDev) {
    const scheme = webLocation?.protocol === 'https:' ? 'https' : 'http'
    const port = env.EXPO_PUBLIC_API_PORT || DEFAULT_DEV_PORT
    return `${scheme}://${devHost()}:${port}${API_PREFIX}`
  }

  // Web production is served behind the same ingress as the API.
  if (webLocation) return `${stripTrailingSlash(webLocation.origin)}${API_PREFIX}`

  // A native release build has no origin to infer and no dev server to ask.
  // Failing loudly at startup beats shipping a placeholder host that turns every
  // request into an unexplained network error.
  throw new Error(
    'EXPO_PUBLIC_API_URL is not set. A native release build cannot infer the API ' +
      'origin — set EXPO_PUBLIC_API_URL (e.g. https://api.example.com/api/v1) in the build environment.',
  )
}

export const API_BASE_URL = resolveApiBase()

function resolveWsBase(): string {
  const fromEnv = env.EXPO_PUBLIC_WS_URL
  if (fromEnv) return stripTrailingSlash(fromEnv)
  // The socket route is GET {API_BASE_URL}/ws. Appending is deliberate: the old
  // `.replace('/api/v1', '/api/v1/ws')` rewrote the FIRST occurrence anywhere in
  // the string, so any origin whose path contained /api/v1 produced a bad URL.
  return `${API_BASE_URL.replace(/^http/i, 'ws')}/ws`
}

export const WS_BASE_URL = resolveWsBase()

/** Default per-request timeout, in milliseconds. */
export const REQUEST_TIMEOUT_MS = 15_000
/** Uploads stream a whole file and need a longer budget. */
export const UPLOAD_TIMEOUT_MS = 60_000
