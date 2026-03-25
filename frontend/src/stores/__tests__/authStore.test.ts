import { describe, it, expect, beforeEach } from 'vitest'
import { useAuthStore } from '../authStore'

describe('authStore', () => {
  beforeEach(() => {
    useAuthStore.setState({
      accessToken: null,
      refreshToken: null,
      user: null,
      isAuthenticated: false,
    })
  })

  it('should set tokens and authenticate', () => {
    useAuthStore.getState().setTokens('access-123', 'refresh-456')

    const state = useAuthStore.getState()
    expect(state.accessToken).toBe('access-123')
    expect(state.refreshToken).toBe('refresh-456')
    expect(state.isAuthenticated).toBe(true)
  })

  it('should set user', () => {
    const user = {
      id: 'u1', email: 'test@test.com', username: 'testuser',
      full_name: 'Test User', avatar_url: '', is_active: true, created_at: '',
    }
    useAuthStore.getState().setUser(user)

    expect(useAuthStore.getState().user?.username).toBe('testuser')
  })

  it('should logout and clear state', () => {
    useAuthStore.getState().setTokens('a', 'b')
    useAuthStore.getState().logout()

    const state = useAuthStore.getState()
    expect(state.accessToken).toBeNull()
    expect(state.refreshToken).toBeNull()
    expect(state.isAuthenticated).toBe(false)
    expect(state.user).toBeNull()
  })
})
