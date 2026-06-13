// @ts-ignore __DEV__ is injected by the RN/Expo runtime
const isDev = typeof __DEV__ !== 'undefined' ? __DEV__ : true
const isWeb = typeof window !== 'undefined' && typeof (window as any).location !== 'undefined'

function resolveApiBase(): string {
  if (isDev) return 'http://localhost:8081/api/v1'
  // Web build served behind the same ingress as the API → use same origin.
  if (isWeb) return `${(window as any).location.origin}/api/v1`
  // Native production builds: configure via EXPO_PUBLIC_API_URL.
  // @ts-ignore process is available via Expo's env injection
  const fromEnv = typeof process !== 'undefined' ? process.env?.EXPO_PUBLIC_API_URL : undefined
  return fromEnv || 'https://your-server.com/api/v1'
}

export const API_BASE_URL = resolveApiBase()

export const WS_BASE_URL = API_BASE_URL
  .replace('https://', 'wss://')
  .replace('http://', 'ws://')
  .replace('/api/v1', '/api/v1/ws')
