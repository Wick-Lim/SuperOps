/**
 * Minimal stand-in for the two `react-native` exports the tested graph touches:
 * `Platform` (secureStorage picks the keystore path from it) and `NativeModules`
 * (config.ts reads `SourceCode.scriptURL` to find the Metro host).
 */

export const Platform = {
  OS: 'ios' as 'ios' | 'android' | 'web',
  select<T>(spec: { ios?: T; android?: T; web?: T; default?: T }): T | undefined {
    return spec.ios ?? spec.default
  },
}

// No SourceCode module: config.ts then falls back to `localhost`, and the
// EXPO_PUBLIC_API_URL set in vitest.config.ts wins over that anyway.
export const NativeModules: Record<string, unknown> = {}
