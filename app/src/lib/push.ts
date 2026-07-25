import { useEffect, useRef } from 'react'
import { Platform } from 'react-native'
import AsyncStorage from '@react-native-async-storage/async-storage'
import * as Notifications from 'expo-notifications'

import { deviceApi, type DevicePlatform } from '../api/devices'
import { channelApi } from '../api/channels'
import { notificationApi } from '../api/notifications'
import { isApiError } from './errors'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUiStore } from '../stores/uiStore'
import { navigationRef } from '../navigation/navigationRef'

/**
 * Expo push notifications.
 *
 * The server half is backend/internal/push; this is the client that gives it an
 * address to send to and decides what happens when one arrives.
 *
 * Three things are worth knowing before changing anything here:
 *
 *  - Push is opt-in server-side (`PUSH_ENABLED`, default false). When it is off
 *    the registration routes do not exist and every call here 404s. That is a
 *    supported configuration, not an error, so it is handled silently.
 *  - Even with `PUSH_ENABLED=true`, delivery additionally requires APNs and FCM
 *    credentials uploaded to the Expo project, plus an `extra.eas.projectId` in
 *    app.json for `getExpoPushTokenAsync` to attribute the token to. Without
 *    those, everything below succeeds and no notification is ever delivered.
 *  - Nothing here is required for the app to work. Every failure path degrades
 *    to "no push", never to a broken screen or a blocked launch.
 */

/**
 * The last token we successfully registered. Kept because logout has to
 * deregister a token that `getExpoPushTokenAsync` may not be able to re-derive
 * at that moment (it is a network call and can fail offline).
 */
const PUSH_TOKEN_KEY = 'superops-push-token'

/**
 * Optional override for the Expo project id. `getExpoPushTokenAsync` normally
 * reads it from `Constants.expoConfig.extra.eas.projectId`; this exists so a
 * build can supply it without editing app.json.
 */
const env: Record<string, string | undefined> =
  typeof process !== 'undefined' && process.env ? (process.env as Record<string, string | undefined>) : {}
const PROJECT_ID = env.EXPO_PUBLIC_EXPO_PROJECT_ID

export type PushOutcome =
  /** A token was obtained and the server accepted it. */
  | 'registered'
  /** The user said no (or had already said no and cannot be asked again). */
  | 'denied'
  /** This platform or this deployment does not do push. Nothing to report. */
  | 'unsupported'
  /** Everything is configured, but the token could not be fetched right now. */
  | 'unavailable'
  /** The server rejected the registration. */
  | 'failed'

function currentPlatform(): DevicePlatform {
  if (Platform.OS === 'ios' || Platform.OS === 'android') return Platform.OS
  return 'web'
}

/**
 * Whether expo-notifications can be called at all on this platform.
 *
 * The package ships a web build, but most of its surface is native-only and
 * throws `ExpoNotifications.<x> is not available on web` when touched — which
 * is a hard crash in a React render, not a degraded feature. Platform.OS is
 * fixed for the process lifetime, so this is a constant.
 */
const pushSupported = Platform.OS === 'ios' || Platform.OS === 'android'

/**
 * `useLastNotificationResponse` is a hook, so it cannot be called conditionally
 * inside the component. Choosing the implementation once at module load keeps
 * exactly one hook in the call order on every platform, while never reaching
 * the native module on web.
 */
const useLastNotificationResponse: typeof Notifications.useLastNotificationResponse =
  pushSupported ? Notifications.useLastNotificationResponse : () => null

/**
 * iOS "provisional" authorization delivers quietly to Notification Center
 * without a prompt. It is a grant, and treating it as a denial would silently
 * disable push for anyone in that state.
 */
function isGranted(status: Notifications.NotificationPermissionsStatus): boolean {
  return status.granted || status.ios?.status === Notifications.IosAuthorizationStatus.PROVISIONAL
}

/** `data` is typed as a string map, but it crosses a native boundary. */
function readData(data: unknown, key: string): string | null {
  if (!data || typeof data !== 'object') return null
  const value = (data as Record<string, unknown>)[key]
  return typeof value === 'string' && value !== '' ? value : null
}

/**
 * Decides how a notification is presented while the app is in the foreground.
 *
 * This is where the "is the user already looking at it?" question belongs, and
 * why the server does not try to answer it. The worker knows only that the
 * recipient has *a* socket open somewhere — which on a multi-device account is
 * routinely a different device — whereas this function knows which channel is
 * on screen right now. Being wrong here costs a redundant banner; being wrong
 * on the server costs a message the user is never told about.
 *
 * Idempotent, and called from `usePushNotifications` on first mount rather than
 * from a module side effect, so importing this module does not reconfigure the
 * OS handler as a side effect of a test or a tree-shake.
 */
let configured = false

export function configureNotifications(): void {
  if (configured) return
  configured = true

  // expo-notifications is a native module with only a partial web shim: the
  // handler/channel APIs below have no web implementation and throw
  // "not available on web". Push is native-only here (see registerPushToken,
  // which reports 'unsupported' for web), so there is nothing to configure.
  if (!pushSupported) return

  Notifications.setNotificationHandler({
    handleNotification: async (notification) => {
      const channelId = readData(notification.request.content.data, 'channel_id')
      const open = useChannelStore.getState().activeChannel
      const looking = channelId !== null && open?.id === channelId

      return {
        shouldShowBanner: !looking,
        shouldShowList: true,
        shouldPlaySound: !looking,
        // The badge is authoritative from the server (it sends the recipient's
        // unread count with every push), so the incoming value is applied.
        shouldSetBadge: true,
      }
    },
  })

  if (Platform.OS === 'android') {
    // Android 8+ ignores per-notification importance; without a channel at
    // HIGH, every message notification is silent and collapsed.
    void Notifications.setNotificationChannelAsync('default', {
      name: 'Messages',
      importance: Notifications.AndroidImportance.HIGH,
      vibrationPattern: [0, 250, 250, 250],
    }).catch(() => {
      // Not fatal — notifications still arrive, just less prominently.
    })
  }
}

/**
 * Asks for permission if needed, obtains an Expo push token and registers it.
 *
 * Deliberately NOT called at launch. A permission prompt on first open, before
 * the user has seen a single message, is the prompt people decline — and iOS
 * only ever asks once. `usePushNotifications` calls this after the workspace
 * bootstrap has succeeded, i.e. once the user is actually in the product.
 */
export async function registerPushToken(): Promise<PushOutcome> {
  // expo-notifications' web support needs a service worker and VAPID keys that
  // this project does not ship; the web client already has the live WebSocket.
  if (currentPlatform() === 'web') return 'unsupported'

  let permissions: Notifications.NotificationPermissionsStatus
  try {
    permissions = await Notifications.getPermissionsAsync()
    if (!isGranted(permissions)) {
      // canAskAgain false means the OS will not show a prompt; calling request
      // anyway resolves immediately with the same denial and, on Android,
      // burns the one-shot rationale.
      if (!permissions.canAskAgain) return 'denied'
      permissions = await Notifications.requestPermissionsAsync({
        ios: { allowAlert: true, allowBadge: true, allowSound: true },
      })
    }
  } catch {
    return 'unavailable'
  }
  if (!isGranted(permissions)) return 'denied'

  let token: string
  try {
    const result = await Notifications.getExpoPushTokenAsync(
      PROJECT_ID ? { projectId: PROJECT_ID } : undefined,
    )
    token = result.data
  } catch {
    // Offline, no project id, or a simulator without push support. Retried on
    // the next launch.
    return 'unavailable'
  }

  try {
    await deviceApi.register(token, currentPlatform())
  } catch (err) {
    // 404 is the deployment saying PUSH_ENABLED is off. Anything else is a
    // real failure, but still not one worth interrupting the user for.
    if (isApiError(err) && err.status === 404) return 'unsupported'
    return 'failed'
  }

  try {
    await AsyncStorage.setItem(PUSH_TOKEN_KEY, token)
  } catch {
    // Only costs us the ability to deregister on logout without a fresh token.
  }
  return 'registered'
}

/**
 * Deregisters this device and forgets the token.
 *
 * Called from `authStore.logout` *before* the session is cleared, because the
 * request needs the access token. Skipping it is what makes the next person to
 * sign in on a shared handset receive the previous user's notifications until
 * they happen to re-register.
 */
export async function deregisterPushToken(accessToken: string | null): Promise<void> {
  let token: string | null = null
  try {
    token = await AsyncStorage.getItem(PUSH_TOKEN_KEY)
  } catch {
    return
  }
  if (!token) return

  await deviceApi.deregister(token, accessToken)
  try {
    await AsyncStorage.removeItem(PUSH_TOKEN_KEY)
  } catch {
    // A stale key is harmless: the next deregister call 404s and moves on.
  }
  await setBadge(0)
}

/** Sets the app icon badge, ignoring launchers that do not support one. */
export async function setBadge(count: number): Promise<void> {
  if (currentPlatform() === 'web') return
  try {
    await Notifications.setBadgeCountAsync(Math.max(0, count))
  } catch {
    // Unsupported launcher, or badge permission not granted on iOS.
  }
}

/** Refreshes the unread count from the server and mirrors it into the badge. */
export async function syncBadge(): Promise<void> {
  try {
    const res = await notificationApi.unreadCount()
    const count = res.data?.count ?? 0
    useUiStore.getState().setUnread(count)
    await setBadge(count)
  } catch {
    // The badge keeps whatever the last push set it to.
  }
}

/**
 * Opens the channel a notification points at.
 *
 * The channel is often not in the sidebar yet — a channel_invite notification
 * arrives before any list refresh — so it is fetched when missing rather than
 * silently doing nothing.
 */
async function openChannel(channelId: string): Promise<void> {
  const known = useChannelStore.getState().channels.find((c) => c.id === channelId)
  if (known) {
    useChannelStore.getState().setActiveChannel(known)
    if (navigationRef.isReady()) navigationRef.navigate('Workspace', {})
    return
  }

  const workspace = useWorkspaceStore.getState().activeWorkspace
  if (!workspace) return
  try {
    const res = await channelApi.get(workspace.id, channelId)
    useChannelStore.getState().addChannel(res.data)
    useChannelStore.getState().setActiveChannel(res.data)
    if (navigationRef.isReady()) navigationRef.navigate('Workspace', {})
  } catch {
    // Left the channel, or it was deleted. The notification list is still
    // reachable from the header.
  }
}

/**
 * Wires push into the app for as long as `enabled` is true.
 *
 * `enabled` is the caller's statement that the user is signed in AND has a
 * workspace — see AppNavigator. Registration runs once per enablement rather
 * than once per mount, so a re-render does not re-prompt.
 */
export function usePushNotifications(enabled: boolean): void {
  const registered = useRef(false)
  const handledResponse = useRef<string | null>(null)

  // Set before anything can arrive, and regardless of `enabled`: without a
  // handler the OS default is to show nothing at all while the app is open.
  useEffect(() => {
    configureNotifications()
  }, [])

  useEffect(() => {
    if (!enabled) {
      // A sign-out (or a workspace-less account) resets the latch so the next
      // sign-in registers again — the token may now belong to a different user.
      registered.current = false
      return
    }
    if (registered.current) return
    registered.current = true

    void (async () => {
      const outcome = await registerPushToken()
      if (outcome === 'registered') {
        // The server sets the badge on every push, but the app may have been
        // reinstalled or read elsewhere since the last one.
        await syncBadge()
      }
    })()
  }, [enabled])

  // Notifications that arrive while the app is open do not change the badge on
  // their own beyond what the push carried; keeping the in-app bell in step is
  // this listener's job.
  useEffect(() => {
    if (!enabled || !pushSupported) return
    const sub = Notifications.addNotificationReceivedListener(() => {
      void syncBadge()
    })
    return () => sub.remove()
  }, [enabled])

  // Taps. `useLastNotificationResponse` also reports the notification that
  // launched a cold-started app, which a plain listener would miss because it
  // fires before this component exists.
  const response = useLastNotificationResponse()
  useEffect(() => {
    if (!enabled || !pushSupported || !response) return
    // Only the default action (an actual tap) navigates; a custom action
    // button, if one is ever added, must not.
    if (response.actionIdentifier !== Notifications.DEFAULT_ACTION_IDENTIFIER) return

    const id = response.notification.request.identifier
    if (handledResponse.current === id) return
    handledResponse.current = id

    const data = response.notification.request.content.data
    const notificationId = readData(data, 'notification_id')
    if (notificationId) {
      // Tapping is reading. Best effort — the list marks it again if this fails.
      notificationApi.markRead(notificationId).then(
        () => void syncBadge(),
        () => {},
      )
    }

    const channelId = readData(data, 'channel_id')
    if (channelId) void openChannel(channelId)
  }, [enabled, response])
}
