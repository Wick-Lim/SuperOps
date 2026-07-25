import React, { useEffect } from 'react'
import { StatusBar } from 'expo-status-bar'
import { View, ActivityIndicator, Text } from 'react-native'
import { GestureHandlerRootView } from 'react-native-gesture-handler'
import { SafeAreaProvider } from 'react-native-safe-area-context'
import { useAuthStore } from './src/stores/authStore'
import AppNavigator from './src/navigation/AppNavigator'
import ErrorBoundary from './src/components/ErrorBoundary'
import ConnectionBanner from './src/components/ConnectionBanner'
import { theme } from './src/lib/theme'

export default function App() {
  const hydrated = useAuthStore((s) => s.hydrated)
  const hydrate = useAuthStore((s) => s.hydrate)

  useEffect(() => {
    hydrate()
  }, [hydrate])

  if (!hydrated) {
    return (
      <View style={{ flex: 1, backgroundColor: theme.bg, justifyContent: 'center', alignItems: 'center', gap: 12 }}>
        <ActivityIndicator size="large" color={theme.primary} />
        <Text accessibilityLiveRegion="polite" style={{ color: theme.textMuted, fontSize: 14 }}>
          Starting…
        </Text>
      </View>
    )
  }

  return (
    // SafeAreaProvider is required by ConnectionBanner: it overlays the top of
    // the screen and has to clear the notch itself, because it sits outside the
    // screens' own SafeAreaViews.
    <SafeAreaProvider>
      <GestureHandlerRootView style={{ flex: 1 }}>
        <StatusBar style="light" />
        <View style={{ flex: 1, backgroundColor: theme.bg }}>
          <ErrorBoundary>
            <AppNavigator />
          </ErrorBoundary>
          {/* Outside the boundary: realtime status stays visible even on the
              crash screen, and the banner itself cannot be what crashed. */}
          <ConnectionBanner />
        </View>
      </GestureHandlerRootView>
    </SafeAreaProvider>
  )
}
