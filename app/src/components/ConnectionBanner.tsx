import React from 'react'
import { ActivityIndicator, Pressable, Text, View } from 'react-native'
import { useSafeAreaInsets } from 'react-native-safe-area-context'
import { useUiStore, type ConnectionStatus } from '../stores/uiStore'
import { wsManager } from '../lib/websocket'
import { theme } from '../lib/theme'
import { MIN_TOUCH } from './a11y'

/**
 * Realtime status.
 *
 * `wsManager.onStatus` had no subscribers and the 1s→30s reconnect backoff ran
 * silently: when the socket dropped, messages simply stopped arriving with no
 * explanation and no way to force a retry. This renders only while the
 * connection is not healthy.
 *
 * It overlays the top of the screen rather than sitting in the layout flow:
 * screens use RN's `SafeAreaView`, which pads by device inset regardless of
 * where it is mounted, so a banner above them would double-inset — and one
 * below them would resize the channel view's `KeyboardAvoidingView`.
 */
const COPY: Partial<Record<ConnectionStatus, { text: string; spinner: boolean; danger: boolean }>> = {
  connecting: { text: 'Connecting…', spinner: true, danger: false },
  reconnecting: { text: 'Reconnecting…', spinner: true, danger: false },
  offline: { text: 'Offline — messages may be delayed', spinner: false, danger: true },
}

export default function ConnectionBanner() {
  // Select the scalar, not an object: a new object identity every render would
  // re-render the whole app on unrelated ui-store writes.
  const status = useUiStore((s) => s.connection)
  const insets = useSafeAreaInsets()

  const copy = COPY[status]
  if (!copy) return null

  const retry = () => {
    wsManager.connect()
    void wsManager.resync('manual')
  }

  return (
    <View
      pointerEvents="box-none"
      style={{ position: 'absolute', top: 0, left: 0, right: 0, paddingTop: insets.top }}
    >
      <View
        accessible
        accessibilityLiveRegion="polite"
        accessibilityRole="alert"
        accessibilityLabel={copy.text}
        style={{
          flexDirection: 'row',
          alignItems: 'center',
          gap: 10,
          paddingHorizontal: 16,
          paddingVertical: 8,
          backgroundColor: copy.danger ? theme.danger : theme.surfaceAlt,
        }}
      >
        {copy.spinner && <ActivityIndicator size="small" color="#fff" />}
        <Text style={{ color: '#fff', fontSize: 13, fontWeight: '600', flex: 1 }} numberOfLines={1}>
          {copy.text}
        </Text>
        {status === 'offline' && (
          <Pressable
            onPress={retry}
            accessibilityRole="button"
            accessibilityLabel="Reconnect now"
            style={{
              minHeight: MIN_TOUCH - 12,
              paddingHorizontal: 12,
              justifyContent: 'center',
              borderRadius: 8,
              backgroundColor: 'rgba(0,0,0,0.25)',
            }}
          >
            <Text style={{ color: '#fff', fontSize: 13, fontWeight: '700' }}>Retry</Text>
          </Pressable>
        )}
      </View>
    </View>
  )
}
