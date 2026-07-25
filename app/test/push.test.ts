/**
 * Regression tests for the web crash:
 *
 *     ExpoNotifications.getLastNotificationResponse is not available on web
 *
 * `usePushNotifications` called `Notifications.useLastNotificationResponse()`
 * unconditionally. On web that hook reaches a native member that does not
 * exist, so it threw during render and took the whole app down — while
 * `tsc --noEmit` and the rest of the suite stayed green, because nothing
 * executed that line outside a browser.
 *
 * The stub at test/stubs/expo-notifications.ts throws from every native member,
 * so any unguarded call fails the test rather than passing silently.
 */
import { describe, expect, it, vi } from 'vitest'

/** Effects queued by the fake `react` below, drained to simulate a mount. */
let pendingEffects: Array<() => void | (() => void)> = []

/**
 * `usePushNotifications` only uses useRef/useEffect, so a real renderer would
 * add a dependency without adding signal. React's ESM namespace cannot be
 * spied on, so the module itself is replaced for the dynamic imports below —
 * `vi.doMock` (not `vi.mock`) because it must not be hoisted above the
 * per-test platform setup.
 */
vi.doMock('react', () => {
  const refs: Array<{ current: unknown }> = []
  let refIndex = 0
  return {
    useRef: (init: unknown) => {
      refs[refIndex] ??= { current: init }
      return refs[refIndex++]
    },
    useEffect: (effect: () => void | (() => void)) => {
      pendingEffects.push(effect)
    },
    __resetRefs: () => {
      refs.length = 0
      refIndex = 0
    },
  }
})

/**
 * `pushSupported` is captured at module load from Platform.OS, so the platform
 * must be set before the import — and on the *fresh* stub instance, because
 * `vi.resetModules()` gives every module (including the react-native stub) a
 * new identity. Mutating the copy imported at the top of this file would be
 * silently discarded, which is a trap worth naming.
 */
async function loadPushOn(os: 'ios' | 'android' | 'web') {
  vi.resetModules()
  const rn = await import('./stubs/react-native')
  rn.Platform.OS = os
  // Same trap as Platform: the notifications stub imported at the top of this
  // file is a DIFFERENT instance from the one push.ts gets after the reset, so
  // its `touched` array would stay empty no matter what the code did — every
  // web assertion would pass vacuously. Read it from the fresh instance.
  const notifications = await import('./stubs/expo-notifications')
  notifications.resetTouched()
  pendingEffects = []
  const push = await import('../src/lib/push')
  return { push, touched: notifications.touched }
}

/** Runs the hook body, then its effects, the way a mount would. */
function renderHook(fn: () => void): void {
  fn()
  const effects = pendingEffects
  pendingEffects = []
  for (const e of effects) e()
}

describe('push on web', () => {
  it('never touches a native notifications member', async () => {
    const { push, touched } = await loadPushOn('web')
    renderHook(() => push.usePushNotifications(true))
    expect(touched).toEqual([])
  })

  it('does not throw when enabled, which is the crash that was reported', async () => {
    const { push, touched } = await loadPushOn('web')
    expect(() => renderHook(() => push.usePushNotifications(true))).not.toThrow()
  })

  it('configureNotifications is a no-op instead of throwing', async () => {
    const { push, touched } = await loadPushOn('web')
    expect(() => push.configureNotifications()).not.toThrow()
    expect(touched).toEqual([])
  })

  it('reports registration as unsupported rather than attempting it', async () => {
    const { push, touched } = await loadPushOn('web')
    await expect(push.registerPushToken()).resolves.toBe('unsupported')
    expect(touched).toEqual([])
  })

  it('setBadge is a no-op', async () => {
    const { push, touched } = await loadPushOn('web')
    await expect(push.setBadge(3)).resolves.toBeUndefined()
    expect(touched).toEqual([])
  })
})

describe('push on native', () => {
  it('does reach the native module — proving the web assertions are not vacuous', async () => {
    const { push, touched } = await loadPushOn('ios')
    // The stub throws on the first native call; that it is reached at all is
    // the point. Without this, a stub that never wired up would make every
    // "touched is empty" assertion above pass for the wrong reason.
    expect(() => push.configureNotifications()).toThrow(/not available on web/)
    expect(touched).toContain('setNotificationHandler')
  })

  it('attempts registration instead of short-circuiting', async () => {
    const { push, touched } = await loadPushOn('android')
    // Rejects because the stub throws, but it got past the platform guard,
    // which is what distinguishes native from web.
    await expect(push.registerPushToken()).resolves.toBe('unavailable')
    expect(touched).toContain('getPermissionsAsync')
  })
})
