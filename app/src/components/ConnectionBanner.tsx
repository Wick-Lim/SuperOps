import React from 'react'
import { ActivityIndicator, Pressable, Text, View } from 'react-native'
import { useSafeAreaInsets } from 'react-native-safe-area-context'
import { useUiStore, type ConnectionStatus } from '../stores/uiStore'
import { wsManager } from '../lib/websocket'
import { theme } from '../lib/theme'
import { MIN_POINTER, space, useResponsive } from '../lib/responsive'

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
 *
 * A full-bleed bar is right on a phone, where it is the width of the content it
 * is talking about. Stretched across a 1600px window it becomes a headline for
 * a footnote, so the pointer tiers get a corner toast instead — same copy, same
 * live region, a twentieth of the screen.
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
  const { tier, gutter } = useResponsive()
  const compact = tier === 'compact'

  const copy = COPY[status]
  if (!copy) return null

  const retry = () => {
    wsManager.connect()
    void wsManager.resync('manual')
  }

  return (
    <View
      pointerEvents="box-none"
      style={{
        position: 'absolute',
        top: 0,
        left: 0,
        right: 0,
        paddingTop: insets.top,
        alignItems: compact ? 'stretch' : 'flex-end',
        paddingHorizontal: compact ? 0 : gutter,
      }}
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
          // Toast geometry above compact: capped, inset from the corner, and
          // outlined so it reads as an overlay rather than as chrome.
          maxWidth: compact ? undefined : 420,
          marginTop: compact ? 0 : space.md,
          borderRadius: compact ? 0 : 10,
          borderWidth: compact ? 0 : 1,
          borderColor: copy.danger ? theme.danger : theme.borderStrong,
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
              // Was 32 — under the pointer floor as well as the touch one.
              minHeight: MIN_POINTER,
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
