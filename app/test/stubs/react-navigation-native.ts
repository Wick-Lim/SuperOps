/**
 * Stand-in for the one `@react-navigation/native` export the tested graph
 * touches: `createNavigationContainerRef`, reached via
 * `src/navigation/navigationRef.ts` when `src/lib/push.ts` imports it.
 *
 * Aliased for the same reason as the other leaf native imports — the real
 * package is shipped untranspiled for Metro, so vitest (which does not
 * transform node_modules) fails to parse it. Nothing here is under test; the
 * push tests only need the module graph to load.
 */

type Listener = (...args: unknown[]) => void

export function createNavigationContainerRef<T = unknown>() {
  const calls: Array<{ name: string; params?: unknown }> = []
  return {
    /** Recorded so a test can assert a notification tap navigated. */
    calls,
    isReady: () => false,
    navigate: (name: string, params?: unknown) => {
      calls.push({ name, params })
    },
    reset: () => {},
    goBack: () => {},
    addListener: (_e: string, _l: Listener) => () => {},
    getCurrentRoute: () => undefined,
  } as unknown as ReturnType<typeof createRefShape<T>>
}

// Only used to give the return value a name in the type above.
declare function createRefShape<T>(): {
  calls: Array<{ name: string; params?: unknown }>
  isReady(): boolean
  navigate(name: string, params?: unknown): void
  reset(): void
  goBack(): void
  addListener(e: string, l: Listener): () => void
  getCurrentRoute(): unknown
} & T
