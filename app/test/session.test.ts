import AsyncStorage from '@react-native-async-storage/async-storage'
import * as SecureStore from 'expo-secure-store'
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { AccountCache } from '../src/lib/accountCache'
import {
  clearDMRosterCache,
  getCachedDMRoster,
  loadDMRoster,
} from '../src/components/channel/dmRosterCache'
import {
  clearCustomEmojiCache,
  getCustomEmojiSnapshot,
  loadCustomEmoji,
} from '../src/components/message/customEmoji'
import {
  clearWorkspaceRoleCache,
  getCachedWorkspaceRole,
  loadWorkspaceRole,
} from '../src/screens/internal/useWorkspaceRole'
import { resetAccountSession, isTerminalBootstrapAuthError } from '../src/lib/accountSession'
import { ApiError } from '../src/lib/errors'
import { registerPushToken, setBadge } from '../src/lib/push'
import { useAuthStore } from '../src/stores/authStore'
import { useChannelStore } from '../src/stores/channelStore'
import { useDriveStore } from '../src/stores/driveStore'
import { useMessageStore } from '../src/stores/messageStore'
import { useUiStore } from '../src/stores/uiStore'
import { useUserStore } from '../src/stores/userStore'
import { useWorkspaceStore } from '../src/stores/workspaceStore'
import { flush, makeChannel, makeMessage, mockFetch, ok, resetStores, signIn } from './helpers'

const notificationMocks = vi.hoisted(() => ({
  getPermissionsAsync: vi.fn(),
  requestPermissionsAsync: vi.fn(),
  getExpoPushTokenAsync: vi.fn(),
  setBadgeCountAsync: vi.fn(),
}))

vi.mock('expo-notifications', () => ({
  ...notificationMocks,
  setNotificationHandler: vi.fn(),
  setNotificationChannelAsync: vi.fn(),
  addNotificationReceivedListener: vi.fn(),
  useLastNotificationResponse: vi.fn(() => null),
  DEFAULT_ACTION_IDENTIFIER: 'default',
  AndroidImportance: { HIGH: 4 },
  IosAuthorizationStatus: { PROVISIONAL: 4 },
}))

const realFetch = globalThis.fetch
const testSecureStore = SecureStore as typeof SecureStore & { __reset: () => void }

beforeEach(async () => {
  notificationMocks.getPermissionsAsync.mockReset().mockResolvedValue({
    granted: true,
    canAskAgain: true,
  })
  notificationMocks.requestPermissionsAsync.mockReset()
  notificationMocks.getExpoPushTokenAsync.mockReset().mockResolvedValue({ data: 'push-shared' })
  notificationMocks.setBadgeCountAsync.mockReset().mockResolvedValue(undefined)
  resetStores()
  await AsyncStorage.clear()
  testSecureStore.__reset()
})

afterEach(() => {
  globalThis.fetch = realFetch
  vi.restoreAllMocks()
})

function populateAccountA(): void {
  signIn('access-a', 'refresh-a', 'user-a')
  useAuthStore.setState({ hydrated: true })
  useWorkspaceStore.setState({
    workspaces: [{ id: 'workspace-a' } as never],
    activeWorkspace: { id: 'workspace-a' } as never,
  })
  useChannelStore.setState({
    channels: [makeChannel('channel-a')],
    activeChannel: makeChannel('channel-a'),
  })
  useMessageStore.setState({
    messages: { 'channel-a': [makeMessage('message-a', 'channel-a')] },
    cursors: { 'channel-a': 'cursor-a' },
    hasMore: { 'channel-a': true },
  })
  useDriveStore.setState({
    registry: [{ type: 'document', creatable: true } as never],
    registryLoaded: true,
    rootId: 'root-a',
    entries: [{ kind: 'file', file: { id: 'file-a' } } as never],
  })
  useUserStore.setState({
    users: { 'user-a': { id: 'user-a', username: 'account-a' } as never },
  })
  useUiStore.setState({
    presence: { 'user-a': 'online' },
    typing: { 'channel-a': ['user-a'] },
    unreadNotifications: 4,
    connection: 'connected',
    activeThread: { channelId: 'channel-a', parent: makeMessage('parent-a', 'channel-a') },
  })
}

describe('generational account caches', () => {
  it('rejects a write holding a generation from before clear', () => {
    const cache = new AccountCache<string, string>()
    const oldGeneration = cache.generation
    cache.clear()
    expect(cache.setIfCurrent(oldGeneration, 'workspace-a', 'old')).toBe(false)
    expect(cache.get('workspace-a')).toBeUndefined()
  })

  it('does not repopulate a cleared custom emoji cache from an old request', async () => {
    let release!: () => void
    mockFetch(async () => {
      await new Promise<void>((resolve) => { release = resolve })
      return ok([{ id: 'emoji-a', name: 'old', image_url: '/old.png' }])
    })

    loadCustomEmoji('workspace-a')
    await flush()
    clearCustomEmojiCache()
    release()
    await flush(20)

    expect(getCustomEmojiSnapshot('workspace-a')).toEqual([])
  })

  it('does not repopulate DM roster or workspace role after clear', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    mockFetch(async (req) => {
      await gate
      if (req.path.includes('/members')) {
        return ok([{ user_id: 'user-a', role: 'admin' }])
      }
      throw new Error(`unexpected request ${req.path}`)
    })

    const roster = loadDMRoster('workspace-a', 'channel-a')
    const role = loadWorkspaceRole('workspace-a', 'user-a')
    await flush()
    clearDMRosterCache()
    clearWorkspaceRoleCache()
    release()
    await Promise.all([roster, role])

    expect(getCachedDMRoster('channel-a')).toBeUndefined()
    expect(getCachedWorkspaceRole('workspace-a:user-a')).toBeUndefined()
  })

  it('resets ChannelView DM state from the newly selected channel before loading', async () => {
    const fs = await import('node:fs/promises')
    const source = await fs.readFile('src/components/channel/ChannelView.tsx', 'utf8')
    const effectStart = source.indexOf('// A DM has `name === null`')
    const effect = source.slice(effectStart, source.indexOf('const title = useMemo', effectStart))
    const reset = effect.indexOf('setDmIds([...(getCachedDMRoster(channel.id) ?? [])])')
    const guard = effect.indexOf('if (!isDM || channel.name) return')

    expect(reset, 'the prior channel roster remains visible after a channel switch').toBeGreaterThan(-1)
    expect(reset, 'the reset must happen even when the new channel is not an unnamed DM').toBeLessThan(guard)
  })
})

describe('account session reset', () => {
  it('empties all account state before remote cleanup settles', async () => {
    populateAccountA()
    await AsyncStorage.setItem('superops-push-token', 'push-a')
    let releaseRemote!: () => void
    const gate = new Promise<void>((resolve) => { releaseRemote = resolve })
    mockFetch(async (req) => {
      if (req.path === '/users/me/devices/push-a' || req.path === '/auth/logout') {
        await gate
        return ok({ message: 'ok' })
      }
      throw new Error(`unexpected request ${req.path}`)
    })

    const resetting = resetAccountSession()
    await flush(20)

    expect(useAuthStore.getState()).toMatchObject({
      accessToken: null,
      refreshToken: null,
      user: null,
      isAuthenticated: false,
      hydrated: true,
    })
    expect(useWorkspaceStore.getState()).toMatchObject({ workspaces: [], activeWorkspace: null })
    expect(useChannelStore.getState()).toMatchObject({ channels: [], activeChannel: null })
    expect(useMessageStore.getState().messages).toEqual({})
    expect(useDriveStore.getState()).toMatchObject({ rootId: null, entries: [] })
    expect(useDriveStore.getState().registry).toHaveLength(1)
    expect(useUserStore.getState().users).toEqual({})
    expect(useUiStore.getState()).toMatchObject({
      presence: {},
      typing: {},
      unreadNotifications: 0,
      connection: 'idle',
      activeThread: null,
    })

    releaseRemote()
    await expect(resetting).resolves.toBeUndefined()
  })

  it('starts B with no selectable state from A and is safe twice', async () => {
    populateAccountA()
    mockFetch(() => ok({ message: 'ok' }))
    await resetAccountSession()
    await resetAccountSession()
    signIn('access-b', 'refresh-b', 'user-b')

    expect(useAuthStore.getState().user?.id).toBe('user-b')
    expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
    expect(useChannelStore.getState().activeChannel).toBeNull()
    expect(useMessageStore.getState().messages['channel-a']).toBeUndefined()
    expect(useUiStore.getState().activeThread).toBeNull()
    expect(useDriveStore.getState().rootId).toBeNull()
    expect(useUserStore.getState().users['user-a']).toBeUndefined()
  })

  it('clears every loaded module cache through the coordinator', async () => {
    signIn('access-a', 'refresh-a', 'user-a')
    mockFetch((req) => {
      if (req.path.includes('/emojis')) {
        return ok([{ id: 'emoji-a', name: 'old', image_url: '/old.png' }])
      }
      if (req.path.includes('/channels/channel-a/members')) {
        return ok([{ user_id: 'user-a' }])
      }
      if (req.path.includes('/workspaces/workspace-a/members')) {
        return ok([{ user_id: 'user-a', role: 'admin' }])
      }
      if (req.path === '/auth/logout') return ok({ message: 'ok' })
      throw new Error(`unexpected request ${req.path}`)
    })

    loadCustomEmoji('workspace-a')
    await Promise.all([
      loadDMRoster('workspace-a', 'channel-a'),
      loadWorkspaceRole('workspace-a', 'user-a'),
    ])
    await flush(20)
    expect(getCustomEmojiSnapshot('workspace-a')).toHaveLength(1)
    expect(getCachedDMRoster('channel-a')).toEqual(['user-a'])
    expect(getCachedWorkspaceRole('workspace-a:user-a')).toBe('admin')

    await resetAccountSession()

    expect(getCustomEmojiSnapshot('workspace-a')).toEqual([])
    expect(getCachedDMRoster('channel-a')).toBeUndefined()
    expect(getCachedWorkspaceRole('workspace-a:user-a')).toBeUndefined()
  })

  it('classifies only terminal bootstrap authorization failures', () => {
    expect(isTerminalBootstrapAuthError(new ApiError(401, 'UNAUTHORIZED', 'expired'))).toBe(true)
    expect(isTerminalBootstrapAuthError(new ApiError(403, 'FORBIDDEN', 'disabled'))).toBe(true)
    expect(isTerminalBootstrapAuthError(new ApiError(0, 'NETWORK_ERROR', 'offline'))).toBe(false)
    expect(isTerminalBootstrapAuthError(new ApiError(503, 'SERVICE_UNAVAILABLE', 'down'))).toBe(false)
  })

  it('keeps the local boundary when storage and remote cleanup fail', async () => {
    populateAccountA()
    await AsyncStorage.setItem('superops-push-token', 'push-a')
    const remove = vi.spyOn(AsyncStorage, 'removeItem').mockRejectedValue(new Error('storage failed'))
    mockFetch(async () => { throw new Error('network failed') })

    await expect(resetAccountSession()).resolves.toBeUndefined()

    expect(useAuthStore.getState().isAuthenticated).toBe(false)
    expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
    expect(useChannelStore.getState().activeChannel).toBeNull()
    expect(useMessageStore.getState().messages).toEqual({})
    expect(useDriveStore.getState().rootId).toBeNull()
    expect(useUiStore.getState().activeThread).toBeNull()
    expect(useUserStore.getState().users).toEqual({})
    remove.mockRestore()
  })

  it('orders an old token write before reset deletion and a new session write', async () => {
    let releaseOldWrite!: () => void
    let enterOldWrite!: () => void
    const oldWriteEntered = new Promise<void>((resolve) => { enterOldWrite = resolve })
    const oldWriteGate = new Promise<void>((resolve) => { releaseOldWrite = resolve })
    const realSetItem = SecureStore.setItemAsync
    vi.spyOn(SecureStore, 'setItemAsync').mockImplementation(async (key, value) => {
      if (key === 'superops.access_token' && value === 'access-a') {
        enterOldWrite()
        await oldWriteGate
      }
      await realSetItem(key, value)
    })
    mockFetch(() => ok({ message: 'ok' }))

    const oldWrite = useAuthStore.getState().setTokens('access-a', 'refresh-a')
    await oldWriteEntered
    const resetting = resetAccountSession()
    const newWrite = useAuthStore.getState().setTokens('access-b', 'refresh-b')
    releaseOldWrite()
    await Promise.all([oldWrite, resetting, newWrite])

    expect(await SecureStore.getItemAsync('superops.access_token')).toBe('access-b')
    expect(await SecureStore.getItemAsync('superops.refresh_token')).toBe('refresh-b')
    expect(useAuthStore.getState()).toMatchObject({
      accessToken: 'access-b',
      refreshToken: 'refresh-b',
      isAuthenticated: true,
    })
  })

  it('cleans up an accepted A push registration when A logs out mid-request', async () => {
    populateAccountA()
    let releaseRegistration!: () => void
    let enterRegistration!: () => void
    const registrationEntered = new Promise<void>((resolve) => { enterRegistration = resolve })
    const registrationGate = new Promise<void>((resolve) => { releaseRegistration = resolve })
    const deviceOperations: string[] = []
    mockFetch(async (req) => {
      if (req.path === '/users/me/devices') {
        deviceOperations.push(`POST ${req.headers.Authorization}`)
        enterRegistration()
        await registrationGate
        return ok({ token: 'push-shared', platform: 'ios' })
      }
      if (req.path === '/users/me/devices/push-shared') {
        deviceOperations.push(`DELETE ${req.headers.Authorization}`)
        return ok({ message: 'ok' })
      }
      if (req.path === '/auth/logout') return ok({ message: 'ok' })
      throw new Error(`unexpected request ${req.path}`)
    })

    const registration = registerPushToken()
    await registrationEntered
    const resetting = resetAccountSession()
    await flush(20)
    releaseRegistration()
    await Promise.all([registration, resetting])

    expect(deviceOperations).toEqual([
      'POST Bearer access-a',
      'DELETE Bearer access-a',
    ])
    expect(await AsyncStorage.getItem('superops-push-token')).toBeNull()
  })

  it('does not let A push cleanup erase B registration for the same device token', async () => {
    populateAccountA()
    await AsyncStorage.setItem('superops-push-token', 'push-shared')
    let releaseAccountA!: () => void
    let enterAccountA!: () => void
    const accountAEntered = new Promise<void>((resolve) => { enterAccountA = resolve })
    const accountAGate = new Promise<void>((resolve) => { releaseAccountA = resolve })
    const deleteBearers: string[] = []
    const network = mockFetch(async (req) => {
      if (req.path === '/users/me/devices/push-shared') {
        deleteBearers.push(req.headers.Authorization)
        if (req.headers.Authorization === 'Bearer access-a') {
          enterAccountA()
          await accountAGate
        }
        return ok({ message: 'ok' })
      }
      if (req.path === '/users/me/devices') {
        expect(req.headers.Authorization).toBe('Bearer access-b')
        return ok({ token: 'push-shared', platform: 'ios' })
      }
      if (req.path === '/auth/logout') return ok({ message: 'ok' })
      throw new Error(`unexpected request ${req.path}`)
    })

    const resettingAccountA = resetAccountSession()
    await accountAEntered
    signIn('access-b', 'refresh-b', 'user-b')
    const registeringAccountB = registerPushToken()
    await flush(20)
    // Registration shares the lifecycle queue with deregistration, so B cannot
    // race A's remote delete for the same physical token.
    expect(network.calls.filter((req) => req.method === 'POST' && req.path === '/users/me/devices')).toHaveLength(0)
    await setBadge(7)
    releaseAccountA()
    await resettingAccountA
    await expect(registeringAccountB).resolves.toBe('registered')

    expect(await AsyncStorage.getItem('superops-push-token')).toBe('push-shared')
    expect(notificationMocks.setBadgeCountAsync).toHaveBeenLastCalledWith(7)
    expect(useAuthStore.getState().user?.id).toBe('user-b')

    await resetAccountSession()

    expect(await AsyncStorage.getItem('superops-push-token')).toBeNull()
    expect(notificationMocks.setBadgeCountAsync).toHaveBeenLastCalledWith(0)
    expect(deleteBearers).toEqual(['Bearer access-a', 'Bearer access-b'])
  })
})
