/**
 * Stand-in for `expo-notifications` that behaves the way the real package does
 * on web: it exists, it imports, and every native-backed member throws the
 * moment it is touched.
 *
 * This is the whole point of the stub. The real failure was
 *
 *     ExpoNotifications.getLastNotificationResponse is not available on web
 *
 * thrown from `useLastNotificationResponse()` during render — the app compiled,
 * the type checker was happy and the rest of the suite passed, because nothing
 * ever executed that line outside a browser. A stub that silently returned
 * `null` on web would reproduce none of that, so every member here is a live
 * grenade and the test asserts it is never pulled.
 */

export class NativeOnlyError extends Error {
  constructor(member: string) {
    super(`ExpoNotifications.${member} is not available on web, are you sure you've linked all the native dependencies properly?`)
    this.name = 'NativeOnlyError'
  }
}

/** Records every native member the code under test reached. */
export const touched: string[] = []

function nativeOnly(member: string) {
  return (..._args: unknown[]): never => {
    touched.push(member)
    throw new NativeOnlyError(member)
  }
}

export function resetTouched(): void {
  touched.length = 0
}

export const setNotificationHandler = nativeOnly('setNotificationHandler')
export const setNotificationChannelAsync = nativeOnly('setNotificationChannelAsync')
export const getPermissionsAsync = nativeOnly('getPermissionsAsync')
export const requestPermissionsAsync = nativeOnly('requestPermissionsAsync')
export const getExpoPushTokenAsync = nativeOnly('getExpoPushTokenAsync')
export const setBadgeCountAsync = nativeOnly('setBadgeCountAsync')
export const addNotificationReceivedListener = nativeOnly('addNotificationReceivedListener')
export const useLastNotificationResponse = nativeOnly('useLastNotificationResponse')

// Plain constants — the real package exposes these on web too, so they must not
// throw or the guards under test would pass for the wrong reason.
export const DEFAULT_ACTION_IDENTIFIER = 'expo.modules.notifications.actions.DEFAULT'
export const AndroidImportance = { HIGH: 4 } as const
export const IosAuthorizationStatus = { PROVISIONAL: 4 } as const

export type NotificationPermissionsStatus = {
  granted: boolean
  ios?: { status: number }
}
