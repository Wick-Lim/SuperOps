import { defineConfig } from 'vitest/config'

/**
 * Vitest (not jest/jest-expo) because the logic under test is plain TypeScript:
 * `api/client.ts`, `lib/websocket.ts` and the zustand stores touch no React
 * Native component and no native module beyond three leaf imports, which are
 * aliased to the stubs below. That means no babel config, no metro
 * transformer and no jest-expo preset — vitest reads the existing tsconfig via
 * esbuild and runs in plain node, which keeps the suite fast and keeps the
 * mocking at the real boundary (`fetch` / `WebSocket`) instead of at module
 * level.
 */
// Absolute path to a stub, resolved against this config file rather than the
// cwd, so the suite runs the same from any directory.
const stub = (p: string) => decodeURIComponent(new URL(p, import.meta.url).pathname)

export default defineConfig({
  resolve: {
    alias: {
      // Leaf native imports pulled in by `src/config.ts`, `src/lib/secureStorage.ts`
      // and `src/stores/authStore.ts`. Nothing else in the tested graph is native.
      'react-native': stub('./test/stubs/react-native.ts'),
      'expo-secure-store': stub('./test/stubs/expo-secure-store.ts'),
      '@react-native-async-storage/async-storage': stub('./test/stubs/async-storage.ts'),
      // Throws on every native member, exactly as the real package does on web.
      // See test/stubs/expo-notifications.ts for why it is not a silent no-op.
      'expo-notifications': stub('./test/stubs/expo-notifications.ts'),
      // Reached from src/lib/push.ts via navigationRef. Shipped untranspiled
      // for Metro, so vitest (which does not transform node_modules) cannot
      // parse it.
      '@react-navigation/native': stub('./test/stubs/react-navigation-native.ts'),
    },
  },
  test: {
    environment: 'node',
    include: ['test/**/*.test.ts'],
    // Pinned so `API_BASE_URL` / `WS_BASE_URL` do not depend on the host that
    // happens to be running the suite (config.ts otherwise sniffs Metro/window).
    env: {
      EXPO_PUBLIC_API_URL: 'http://api.test/api/v1',
      EXPO_PUBLIC_WS_URL: 'ws://api.test/api/v1/ws',
    },
  },
})
