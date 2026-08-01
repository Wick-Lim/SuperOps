import { authApi } from '../api/auth'
import { api } from '../api/client'
import { clearDMRosterCache } from '../components/channel/dmRosterCache'
import { clearCustomEmojiCache } from '../components/message/customEmoji'
import { clearWorkspaceRoleCache } from '../screens/internal/useWorkspaceRole'
import { clearLocalAuthSession, useAuthStore } from '../stores/authStore'
import { useChannelStore } from '../stores/channelStore'
import { useDriveStore } from '../stores/driveStore'
import { useMessageStore } from '../stores/messageStore'
import { useUiStore } from '../stores/uiStore'
import { useUserStore } from '../stores/userStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { isApiError } from './errors'
import { wsManager } from './websocket'

export function isTerminalBootstrapAuthError(error: unknown): boolean {
  return isApiError(error) && (error.status === 401 || error.status === 403)
}

export async function resetAccountSession(): Promise<void> {
  const { accessToken, refreshToken } = useAuthStore.getState()

  api.resetSession()
  const localResetGeneration = useAuthStore.getState().sessionGeneration
  wsManager.reset()
  useWorkspaceStore.getState().clear()
  useChannelStore.getState().clear()
  useMessageStore.getState().clear()
  useDriveStore.getState().clear()
  useUserStore.getState().clear()
  useUiStore.getState().clear()
  clearDMRosterCache()
  clearCustomEmojiCache()
  clearWorkspaceRoleCache()

  // This synchronous set is deliberately last: once observers see false,
  // every other account selector above is already empty.
  const localStorageCleanup = clearLocalAuthSession()

  const pushCleanup = import('./push').then(({ deregisterPushToken }) =>
    deregisterPushToken(accessToken, localResetGeneration),
  )
  const serverCleanup = refreshToken
    ? authApi.logout(refreshToken).then(() => undefined)
    : Promise.resolve()

  await Promise.allSettled([localStorageCleanup, pushCleanup, serverCleanup])
}
