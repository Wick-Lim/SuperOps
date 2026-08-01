import { create } from 'zustand'
import AsyncStorage from '@react-native-async-storage/async-storage'
import type { User } from '../lib/types'
import { getSecureItem, setSecureItem, deleteSecureItem } from '../lib/secureStorage'
import { authApi } from '../api/auth'

// expo-secure-store keys must match [A-Za-z0-9._-]+.
const ACCESS_KEY = 'superops.access_token'
const REFRESH_KEY = 'superops.refresh_token'
// The cached profile is not a credential and can exceed the keystore's value
// limit, so it stays in AsyncStorage.
const USER_KEY = 'superops-user'
// Pre-keystore location of the token pair; read once, then migrated and erased.
const LEGACY_AUTH_KEY = 'superops-auth'

interface AuthState {
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
  logout: (expectedSession?: { accessToken: string | null; refreshToken: string | null }) => Promise<void>
  hydrate: () => Promise<void>
}

export const useAuthStore = create<AuthState>()((set, get) => ({
  accessToken: null,
  refreshToken: null,
  user: null,
  isAuthenticated: false,
  hydrated: false,
  persistError: null,

  // Awaited, not fire-and-forget: a failed write used to be invisible and
  // silently signed the user out on the next cold start.
  setTokens: async (access, refresh) => {
    set({ accessToken: access, refreshToken: refresh, isAuthenticated: true })
    try {
      await setSecureItem(ACCESS_KEY, access)
      await setSecureItem(REFRESH_KEY, refresh)
      if (get().persistError) set({ persistError: null })
    } catch (err) {
      set({ persistError: err instanceof Error ? err.message : 'Could not save your session securely.' })
    }
  },

  setUser: async (user) => {
    set({ user })
    try {
      await AsyncStorage.setItem(USER_KEY, JSON.stringify(user))
    } catch {
      // The profile is a cache; losing it only costs one extra /users/me call.
    }
  },

  logout: async (expectedSession) => {
    // Captured before clearing: the revoke call needs the token, and the route
    // (POST /api/v1/auth/logout) is unauthenticated by design.
    const targetSession = expectedSession ?? {
      accessToken: get().accessToken,
      refreshToken: get().refreshToken,
    }
    const refresh = targetSession.refreshToken
    const access = targetSession.accessToken
    const isTargetSessionCurrent = () => {
      const current = get()
      return current.accessToken === access && current.refreshToken === refresh
    }

    const revoke = async () => {
      if (!refresh) return
      try {
        await authApi.logout(refresh)
      } catch {
        // An offline logout is still a logout locally.
      }
    }

    // A caller may have crossed a reset before reaching the store. Never let
    // its cleanup clear the account that replaced it.
    if (!isTargetSessionCurrent()) {
      await revoke()
      return
    }

    // Deregister this device's push token BEFORE the session is cleared — the
    // DELETE needs the access token. Without it, the next person to sign in on
    // a shared handset keeps receiving this user's notifications until they
    // happen to register the same token themselves.
    //
    // Imported dynamically, and failure ignored, for two reasons: `lib/push`
    // pulls in expo-notifications, which must not become a static dependency of
    // this store (the unit suite runs it in plain node), and a device that
    // cannot be deregistered must not be able to block a sign-out.
    try {
      const { deregisterPushToken } = await import('../lib/push')
      await deregisterPushToken(access)
    } catch {
      // No push module, no permission, or offline. Signing out still proceeds.
    }

    // Push cleanup is asynchronous. The user may have signed into a different
    // account while it was running, in which case this logout now belongs to
    // the old session and must not touch the replacement's local state.
    if (!isTargetSessionCurrent()) {
      await revoke()
      return
    }

    set({ accessToken: null, refreshToken: null, user: null, isAuthenticated: false, persistError: null })

    await Promise.all([
      deleteSecureItem(ACCESS_KEY),
      deleteSecureItem(REFRESH_KEY),
      AsyncStorage.removeItem(USER_KEY).catch(() => {}),
      AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => {}),
    ])

    // Best effort: without this the refresh-token row survives every
    // user-initiated logout until it expires naturally.
    await revoke()
  },

  hydrate: async () => {
    try {
      let access = await getSecureItem(ACCESS_KEY)
      let refresh = await getSecureItem(REFRESH_KEY)

      // Migrate a pre-keystore session instead of forcing a re-login.
      if (!access) {
        const legacy = await AsyncStorage.getItem(LEGACY_AUTH_KEY)
        if (legacy) {
          try {
            const parsed = JSON.parse(legacy) as { accessToken?: string; refreshToken?: string }
            if (parsed?.accessToken) {
              access = parsed.accessToken
              refresh = parsed.refreshToken ?? null
              await setSecureItem(ACCESS_KEY, access)
              if (refresh) await setSecureItem(REFRESH_KEY, refresh)
            }
          } catch {
            // Unparsable legacy blob — treat as signed out.
          }
          await AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => {})
        }
      }

      let user: User | null = null
      const userStr = await AsyncStorage.getItem(USER_KEY)
      if (userStr) {
        try {
          user = JSON.parse(userStr) as User
        } catch {
          user = null
        }
      }

      if (access) {
        set({ accessToken: access, refreshToken: refresh, user, isAuthenticated: true, hydrated: true })
      } else {
        set({ hydrated: true })
      }
    } catch {
      set({ hydrated: true })
    }
  },
}))
