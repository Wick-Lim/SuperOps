import React from 'react'
import { View, Text, Pressable, StyleSheet } from 'react-native'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import type { HuddleSession } from '../api/huddles'

/**
 * Joining a call on a phone: NOT SUPPORTED, and it says so.
 *
 * ROADMAP §3's mobile dev-build path is the explicit cut (plan 07's cut table,
 * "mobile dev-build path"): joining needs livekit-client's WebRTC, which needs
 * native modules that an Expo Go build does not have, so shipping it here would
 * mean either a custom dev client for every user or a Join button that throws.
 *
 * What mobile DOES get is everything around the call — the bar, the live
 * roster, start and end — because the cut is the media, not the feature.
 * Nothing here imports livekit-client, which is the whole reason this file
 * exists and what CI's bundle check verifies.
 */

export interface HuddleRoomProps {
  session: HuddleSession
  onLeave: () => void
}

export default function HuddleRoom({ onLeave }: HuddleRoomProps) {
  return (
    <View style={styles.bar}>
      <Text style={styles.text}>Join this huddle from the web app.</Text>
      <Pressable
        onPress={onLeave}
        style={({ pressed }) => [styles.close, pressed && styles.pressed]}
        accessibilityRole="button"
      >
        <Text style={styles.closeText}>Close</Text>
      </Pressable>
    </View>
  )
}

const styles = StyleSheet.create({
  bar: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingVertical: space.sm,
    backgroundColor: theme.surfaceAlt,
  },
  text: { color: theme.textMuted, fontSize: 13, flex: 1 },
  close: { minHeight: MIN_TOUCH - 12, justifyContent: 'center', paddingHorizontal: space.sm },
  closeText: { color: theme.body, fontSize: 13, fontWeight: '600' },
  pressed: { opacity: 0.7 },
})
