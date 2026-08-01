import { create } from 'zustand'
import AsyncStorage from '@react-native-async-storage/async-storage'
import type { User } from '../lib/types'
import { getSecureItem, setSecureItem, deleteSecureItem } from '../lib/secureStorage'

// expo-secure-store keys must match [A-Za-z0-9._-]+.
const ACCESS_KEY = 'superops.access_token'
const REFRESH_KEY = 'superops.refresh_token'
// The cached profile is not a credential and can exceed the keystore's value
// limit, so it stays in AsyncStorage.
const USER_KEY = 'superops-user'
// Pre-keystore location of the token pair; read once, then migrated and erased.
const LEGACY_AUTH_KEY = 'superops-auth'

let authSessionGeneration = 0
let persistenceTail: Promise<void> = Promise.resolve()

function enqueuePersistence<T>(work: () => Promise<T>): Promise<T> {
  const run = persistenceTail.then(work, work)
  persistenceTail = run.then(() => undefined, () => undefined)
  return run
}

export function clearLocalAuthSession(): Promise<void> {
  authSessionGeneration += 1
  useAuthStore.setState({
    accessToken: null,
    refreshToken: null,
    user: null,
    isAuthenticated: false,
    persistError: null,
  })
  return enqueuePersistence(async () => {
    await Promise.all([
      deleteSecureItem(ACCESS_KEY),
      deleteSecureItem(REFRESH_KEY),
      AsyncStorage.removeItem(USER_KEY).catch(() => undefined),
      AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => undefined),
    ])
  })
}

interface StoredAuthSession {
  access: string | null
  refresh: string | null
  user: User | null
}

async function readStoredAuthSession(): Promise<StoredAuthSession> {
  let access = await getSecureItem(ACCESS_KEY)
  let refresh = await getSecureItem(REFRESH_KEY)
  if (!access) {
    const legacy = await AsyncStorage.getItem(LEGACY_AUTH_KEY)
    if (legacy) {
      try {
        const parsed = JSON.parse(legacy) as { accessToken?: string; refreshToken?: string }
        if (parsed.accessToken) {
          access = parsed.accessToken
          refresh = parsed.refreshToken ?? null
          await setSecureItem(ACCESS_KEY, access)
          if (refresh) await setSecureItem(REFRESH_KEY, refresh)
        }
      } catch {
        access = null
        refresh = null
      }
      await AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => undefined)
    }
  }
  let user: User | null = null
  const serialized = await AsyncStorage.getItem(USER_KEY)
  if (serialized) {
    try {
      user = JSON.parse(serialized) as User
    } catch {
      user = null
    }
  }
  return { access, refresh, user }
}

interface AuthState {
  /** Changes only when an account session ends, never for same-session token rotation. */
  sessionGeneration: number
  accessToken: string | null
  refreshToken: string | null
  user: User | null
  isAuthenticated: boolean
  hydrated: boolean
  /** Last persistence failure, if any — a silent one logs the user out next launch. */
  persistError: string | null

  setTokens: (access: string, refresh: string) => Promise<void>
  setUser: (user: User) => Promise<void>
  /** Clears the local session and best-effort revokes the server refresh token. */
  advanceSessionGeneration: () => void
  logout: (expectedSession?: {
    generation: number
    accessToken: string | null
    refreshToken: string | null
  }) => Promise<void>
  hydrate: () => Promise<void>
}

export const useAuthStore = create<AuthState>()((set, get) => ({
  sessionGeneration: 0,
  accessToken: null,
  refreshToken: null,
  user: null,
  isAuthenticated: false,
  hydrated: false,
  persistError: null,

  advanceSessionGeneration: () => set((state) => ({ sessionGeneration: state.sessionGeneration + 1 })),

  // Awaited, not fire-and-forget: a failed write used to be invisible and
  // silently signed the user out on the next cold start.
  setTokens: async (access, refresh) => {
    const generation = authSessionGeneration
    set({ accessToken: access, refreshToken: refresh, isAuthenticated: true })
    try {
      await enqueuePersistence(async () => {
        if (generation !== authSessionGeneration) return
        await setSecureItem(ACCESS_KEY, access)
        await setSecureItem(REFRESH_KEY, refresh)
      })
      if (generation === authSessionGeneration && get().persistError) {
        set({ persistError: null })
      }
    } catch (error) {
      if (generation === authSessionGeneration) {
        set({ persistError: error instanceof Error ? error.message : 'Could not save your session securely.' })
      }
    }
  },

  setUser: async (user) => {
    const generation = authSessionGeneration
    set({ user })
    try {
      await enqueuePersistence(async () => {
        if (generation !== authSessionGeneration) return
        await AsyncStorage.setItem(USER_KEY, JSON.stringify(user))
      })
    } catch {
      // The profile is a cache; the authenticated profile endpoint refills it.
    }
  },

  logout: async (expectedSession) => {
    if (expectedSession && get().sessionGeneration !== expectedSession.generation) return
    const { resetAccountSession } = await import('../lib/accountSession')
    if (expectedSession && get().sessionGeneration !== expectedSession.generation) return
    await resetAccountSession()
  },

  hydrate: async () => {
    const generation = authSessionGeneration
    try {
      const stored = await enqueuePersistence(readStoredAuthSession)
      if (generation !== authSessionGeneration) return
      if (stored.access) {
        set({
          accessToken: stored.access,
          refreshToken: stored.refresh,
          user: stored.user,
          isAuthenticated: true,
          hydrated: true,
        })
      } else {
        set({ hydrated: true })
      }
    } catch {
      if (generation === authSessionGeneration) set({ hydrated: true })
    }
  },
}))
