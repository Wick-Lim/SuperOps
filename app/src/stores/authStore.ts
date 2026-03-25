import { create } from 'zustand'
import AsyncStorage from '@react-native-async-storage/async-storage'
import type { User } from '../lib/types'

interface AuthState {
  accessToken: string | null
  refreshToken: string | null
  user: User | null
  isAuthenticated: boolean
  hydrated: boolean
  setTokens: (access: string, refresh: string) => void
  setUser: (user: User) => void
  logout: () => void
  hydrate: () => Promise<void>
}

export const useAuthStore = create<AuthState>()((set) => ({
  accessToken: null,
  refreshToken: null,
  user: null,
  isAuthenticated: false,
  hydrated: false,
  setTokens: (access, refresh) => {
    set({ accessToken: access, refreshToken: refresh, isAuthenticated: true })
    AsyncStorage.setItem('superops-auth', JSON.stringify({ accessToken: access, refreshToken: refresh }))
  },
  setUser: (user) => {
    set({ user })
    AsyncStorage.setItem('superops-user', JSON.stringify(user))
  },
  logout: () => {
    set({ accessToken: null, refreshToken: null, user: null, isAuthenticated: false })
    AsyncStorage.multiRemove(['superops-auth', 'superops-user'])
  },
  hydrate: async () => {
    try {
      const [authStr, userStr] = await AsyncStorage.multiGet(['superops-auth', 'superops-user'])
      const auth = authStr[1] ? JSON.parse(authStr[1]) : null
      const user = userStr[1] ? JSON.parse(userStr[1]) : null
      if (auth?.accessToken) {
        set({ accessToken: auth.accessToken, refreshToken: auth.refreshToken, user, isAuthenticated: true, hydrated: true })
      } else {
        set({ hydrated: true })
      }
    } catch {
      set({ hydrated: true })
    }
  },
}))
