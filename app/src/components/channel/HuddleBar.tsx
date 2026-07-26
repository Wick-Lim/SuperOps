import React, { useCallback, useEffect, useState } from 'react'
import { View, Text, Pressable, StyleSheet, Alert } from 'react-native'
import { theme } from '../../lib/theme'
import { space, MIN_TOUCH } from '../../lib/responsive'
import { errorMessage } from '../../api/client'
import { huddleApi } from '../../api/huddles'
import type { HuddleSession } from '../../api/huddles'
import { wsManager } from '../../lib/websocket'
import HuddleRoom from '../../huddle/HuddleRoom'

/**
 * The huddle bar.
 *
 * IT ASKS THE SERVER WHETHER CALLS EXIST, once, and renders nothing at all if
 * they do not. Huddles are a deployment-dependent capability: an SFU is a
 * second server somebody has to run, and a button that 404s is worse than no
 * button. A hardcoded "huddles are on" would be wrong on exactly the
 * deployments that matter.
 *
 * The roster comes from the pull path. The WS frame is a NUDGE that says
 * something changed — sending the roster itself would mean deciding per
 * subscriber what they may see, which GET /huddle already does correctly.
 *
 * WHAT THIS DOES NOT DO IS JOIN THE MEDIA. Connecting to the SFU needs
 * livekit-client, ~200 KB of WebRTC that only the web bundle can use, and the
 * plan's own cut is that mobile does not join. So the bar shows the call, its
 * roster and the controls that need no media — start, end, and open — and
 * hands the join off. That is an honest partial rather than a Join button that
 * does nothing on half the platforms.
 */
export default function HuddleBar({ channelId, canWrite }: { channelId: string; canWrite: boolean }) {
  // `null` means "not asked yet", `false` means "this deployment has no media
  // server". The three states are distinct because rendering an empty bar for
  // the second one would leave a gap in the layout forever.
  const [available, setAvailable] = useState<boolean | null>(null)
  const [session, setSession] = useState<HuddleSession | null>(null)
  const [busy, setBusy] = useState(false)
  // Joined is separate from "a call exists": somebody may be looking at a
  // channel with a live huddle they have not joined, which is the common case.
  const [joined, setJoined] = useState(false)

  const refresh = useCallback(async () => {
    try {
      const res = await huddleApi.get(channelId)
      const data = res.data as HuddleSession | { huddle: null } | undefined
      setAvailable(true)
      setSession(data && 'token' in data && data.huddle ? (data as HuddleSession) : null)
    } catch (e) {
      const status = (e as { status?: number })?.status
      if (status === 404 || status === 501) {
        setAvailable(false)
        return
      }
      // A transient failure must not hide a live call permanently; leave the
      // last known state and try again on the next event.
      setAvailable((cur) => cur ?? true)
    }
  }, [channelId])

  useEffect(() => {
    void refresh()
  }, [refresh])

  // The three frames the client used to drop on the floor.
  useEffect(() => {
    // Wrapped so the effect returns void: Set.delete answers a boolean, and
    // React treats any non-function return as a mistake.
    const off = wsManager.onHuddle((e) => {
      if (e.channelId !== channelId) return
      void refresh()
    })
    return () => {
      off()
    }
  }, [channelId, refresh])

  const start = useCallback(async () => {
    setBusy(true)
    try {
      const res = await huddleApi.start(channelId)
      setSession(res.data ?? null)
      // Starting a call joins it. Anything else would leave the person who
      // clicked "Start huddle" outside their own huddle.
      setJoined(true)
    } catch (e) {
      Alert.alert('Could not start a huddle', errorMessage(e))
    } finally {
      setBusy(false)
    }
  }, [channelId])

  const end = useCallback(async () => {
    Alert.alert('End the huddle?', 'It ends for everybody in it.', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'End',
        style: 'destructive',
        onPress: async () => {
          try {
            await huddleApi.end(channelId)
            setSession(null)
            setJoined(false)
          } catch (e) {
            Alert.alert('Could not end the huddle', errorMessage(e))
          }
        },
      },
    ])
  }, [channelId])

  if (available !== true) return null

  if (!session?.huddle) {
    return (
      <View style={styles.bar}>
        <Text style={styles.idle}>No one is in a huddle.</Text>
        <Pressable
          onPress={start}
          disabled={busy || !canWrite}
          style={({ pressed }) => [styles.action, (pressed || busy || !canWrite) && styles.pressed]}
          accessibilityRole="button"
          accessibilityLabel="Start a huddle in this channel"
        >
          <Text style={styles.actionText}>{busy ? 'Starting' : 'Start huddle'}</Text>
        </Pressable>
      </View>
    )
  }

  const live = session.participants.filter((p) => p.left_at === null)
  const sharing = live.some((p) => p.is_screen_sharing)

  if (joined) {
    return <HuddleRoom session={session} onLeave={() => setJoined(false)} />
  }

  return (
    <View style={[styles.bar, styles.liveBar]}>
      <View style={styles.dot} />
      <Text style={styles.liveText}>
        {live.length === 0 ? 'Huddle starting' : `${live.length} in a huddle`}
        {sharing ? ' · sharing a screen' : ''}
        {/* The server decides. A read-capability caller joins muted and the
            token is what enforces it — saying so up front is better than a
            microphone button that silently does nothing. */}
        {session.can_publish ? '' : ' · you can listen'}
      </Text>

      {session.url ? (
        <Pressable
          onPress={async () => {
            // Re-fetch first: the token is short-lived — it IS the grant, and a
            // revoked share does not invalidate one already issued — so the one
            // fetched when the bar rendered may have expired while somebody
            // read the channel.
            try {
              const res = await huddleApi.start(channelId)
              setSession(res.data ?? session)
            } catch {
              /* join with what we have; the SFU will refuse a stale token */
            }
            setJoined(true)
          }}
          style={({ pressed }) => [styles.action, pressed && styles.pressed]}
          accessibilityRole="button"
          accessibilityLabel="Join the huddle"
        >
          <Text style={styles.actionText}>Join</Text>
        </Pressable>
      ) : null}

      {canWrite && (
        <Pressable
          onPress={end}
          style={({ pressed }) => [styles.end, pressed && styles.pressed]}
          accessibilityRole="button"
          accessibilityLabel="End the huddle"
        >
          <Text style={styles.endText}>End</Text>
        </Pressable>
      )}
    </View>
  )
}

const styles = StyleSheet.create({
  bar: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingVertical: space.xs,
    borderBottomWidth: StyleSheet.hairlineWidth,
    borderBottomColor: theme.border,
    backgroundColor: theme.surface,
  },
  liveBar: { backgroundColor: theme.surfaceAlt },
  dot: { width: 8, height: 8, borderRadius: 4, backgroundColor: theme.success },
  idle: { color: theme.textFaint, fontSize: 13, flex: 1 },
  liveText: { color: theme.body, fontSize: 13, flex: 1 },
  action: {
    minHeight: MIN_TOUCH - 12,
    justifyContent: 'center',
    paddingHorizontal: space.sm,
    borderRadius: 6,
    backgroundColor: theme.primary,
  },
  actionText: { color: theme.primaryText, fontSize: 13, fontWeight: '600' },
  end: { minHeight: MIN_TOUCH - 12, justifyContent: 'center', paddingHorizontal: space.sm },
  endText: { color: theme.danger, fontSize: 13, fontWeight: '600' },
  pressed: { opacity: 0.7 },
})
