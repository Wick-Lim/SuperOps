import React, { useCallback, useEffect, useRef, useState } from 'react'
import { View, Text, Pressable, StyleSheet } from 'react-native'
import {
  Room, RoomEvent, Track, ConnectionState,
  type RemoteTrack, type RemoteParticipant, type LocalTrackPublication,
} from 'livekit-client'
import { theme } from '../lib/theme'
import { space, MIN_TOUCH } from '../lib/responsive'
import type { HuddleSession } from '../api/huddles'

/**
 * Joining the call. WEB ONLY.
 *
 * livekit-client is roughly 200 KB of WebRTC that the mobile bundle has no use
 * for — ROADMAP §3's mobile dev-build path is the cut, not this — so it lives
 * behind the same platform split as the block editor, and CI's bundle check is
 * what keeps it there.
 *
 * THREE THINGS HERE ARE NOT UI POLISH:
 *
 *   - The token decides what may be published. A read-capability caller joins
 *     with `can_publish: false` and the SFU refuses their microphone; the
 *     control is DISABLED rather than failing on press, because a button that
 *     does nothing is worse than one that is visibly unavailable.
 *   - Audio elements are attached to the DOM and detached on unsubscribe. A
 *     detached element keeps playing, so somebody who left a call would still
 *     be audible — the single most common bug in a first WebRTC integration.
 *   - Disconnect on unmount, unconditionally. A room that survives navigation
 *     keeps the microphone open, and the browser's recording indicator stays on
 *     while the person believes they hung up.
 */

export interface HuddleRoomProps {
  session: HuddleSession
  onLeave: () => void
}

type Status = 'connecting' | 'connected' | 'reconnecting' | 'failed'

export default function HuddleRoom({ session, onLeave }: HuddleRoomProps) {
  const roomRef = useRef<Room | null>(null)
  const audioHost = useRef<HTMLDivElement | null>(null)
  const [status, setStatus] = useState<Status>('connecting')
  const [error, setError] = useState<string | null>(null)
  const [muted, setMuted] = useState(true)
  const [sharing, setSharing] = useState(false)
  const [peers, setPeers] = useState<string[]>([])

  useEffect(() => {
    if (!session.url || !session.token) {
      setError('This deployment has no media server.')
      setStatus('failed')
      return
    }

    const room = new Room({
      // Adaptive streaming and dynacast cut bandwidth for people who are not
      // being looked at. With audio and screen share only they matter less than
      // they would with camera video, but a ten-person room still carries one
      // screen stream nobody but the speaker needs at full rate.
      adaptiveStream: true,
      dynacast: true,
    })
    roomRef.current = room

    const attach = (track: RemoteTrack) => {
      // Audio only — there is no camera in v1, and a screen share is rendered
      // by the browser's own picture-in-picture rather than a tile grid we
      // would have to build.
      if (track.kind !== Track.Kind.Audio) return
      const el = track.attach()
      el.setAttribute('data-huddle-audio', '')
      audioHost.current?.appendChild(el)
    }

    const detach = (track: RemoteTrack) => {
      // DETACH, not just remove from the DOM. A detached-but-playing element is
      // how somebody who hung up stays audible.
      track.detach().forEach((el) => el.remove())
    }

    const refreshPeers = () => {
      setPeers([...room.remoteParticipants.values()].map((p: RemoteParticipant) => p.identity))
    }

    room
      .on(RoomEvent.TrackSubscribed, attach)
      .on(RoomEvent.TrackUnsubscribed, detach)
      .on(RoomEvent.ParticipantConnected, refreshPeers)
      .on(RoomEvent.ParticipantDisconnected, refreshPeers)
      .on(RoomEvent.ConnectionStateChanged, (state: ConnectionState) => {
        switch (state) {
          case ConnectionState.Connected:
            setStatus('connected')
            setError(null)
            break
          case ConnectionState.Reconnecting:
            // Said out loud. A silent reconnect looks like everybody suddenly
            // stopped talking.
            setStatus('reconnecting')
            break
          case ConnectionState.Disconnected:
            setStatus('failed')
            break
        }
      })
      .on(RoomEvent.LocalTrackPublished, (pub: LocalTrackPublication) => {
        if (pub.source === Track.Source.ScreenShare) setSharing(true)
      })
      .on(RoomEvent.LocalTrackUnpublished, (pub: LocalTrackPublication) => {
        if (pub.source === Track.Source.ScreenShare) setSharing(false)
      })

    void room
      .connect(session.url, session.token)
      .then(() => {
        setStatus('connected')
        refreshPeers()
      })
      .catch((e: unknown) => {
        setStatus('failed')
        setError(e instanceof Error ? e.message : 'Could not join the call')
      })

    return () => {
      // UNCONDITIONAL. A room that survives navigation keeps the microphone
      // open and the browser's recording indicator lit while the person
      // believes they hung up.
      void room.disconnect()
      roomRef.current = null
    }
  }, [session.url, session.token])

  const toggleMic = useCallback(async () => {
    const room = roomRef.current
    if (!room || !session.can_publish) return
    try {
      const next = !muted
      await room.localParticipant.setMicrophoneEnabled(!next)
      setMuted(next)
    } catch (e) {
      setError(e instanceof Error ? e.message : 'Could not change the microphone')
    }
  }, [muted, session.can_publish])

  const toggleShare = useCallback(async () => {
    const room = roomRef.current
    if (!room || !session.can_publish) return
    try {
      await room.localParticipant.setScreenShareEnabled(!sharing)
    } catch (e) {
      // A cancelled picker throws, and it is not an error worth showing: the
      // person simply changed their mind.
      if ((e as { name?: string })?.name !== 'NotAllowedError') {
        setError(e instanceof Error ? e.message : 'Could not share the screen')
      }
    }
  }, [sharing, session.can_publish])

  const leave = useCallback(() => {
    void roomRef.current?.disconnect()
    onLeave()
  }, [onLeave])

  return (
    <View style={styles.root}>
      {/* Remote audio lives here. Rendered through a ref rather than React
          children because the tracks attach real <audio> elements. */}
      <div ref={audioHost} style={{ display: 'none' }} />

      <View style={styles.bar}>
        <View style={[styles.dot, status === 'connected' ? styles.dotLive : styles.dotIdle]} />
        <Text style={styles.status}>
          {status === 'connecting' && 'Joining'}
          {status === 'connected' && `${peers.length + 1} in the call`}
          {status === 'reconnecting' && 'Reconnecting'}
          {status === 'failed' && (error ?? 'Disconnected')}
        </Text>

        <Pressable
          onPress={toggleMic}
          disabled={!session.can_publish || status !== 'connected'}
          style={({ pressed }) => [
            styles.control,
            (!session.can_publish || status !== 'connected') && styles.disabled,
            pressed && styles.pressed,
          ]}
          accessibilityRole="button"
          accessibilityLabel={muted ? 'Unmute' : 'Mute'}
        >
          {/* The SERVER decided this. A read-capability caller can listen —
              a real product state — and the token is what enforces it, so the
              control says so instead of failing on press. */}
          <Text style={styles.controlText}>
            {!session.can_publish ? 'Listening' : muted ? 'Unmute' : 'Mute'}
          </Text>
        </Pressable>

        <Pressable
          onPress={toggleShare}
          disabled={!session.can_publish || status !== 'connected'}
          style={({ pressed }) => [
            styles.control,
            (!session.can_publish || status !== 'connected') && styles.disabled,
            pressed && styles.pressed,
          ]}
          accessibilityRole="button"
          accessibilityLabel={sharing ? 'Stop sharing' : 'Share your screen'}
        >
          <Text style={styles.controlText}>{sharing ? 'Stop sharing' : 'Share screen'}</Text>
        </Pressable>

        <Pressable
          onPress={leave}
          style={({ pressed }) => [styles.leave, pressed && styles.pressed]}
          accessibilityRole="button"
        >
          <Text style={styles.leaveText}>Leave</Text>
        </Pressable>
      </View>
    </View>
  )
}

const styles = StyleSheet.create({
  root: { backgroundColor: theme.surfaceAlt },
  bar: {
    flexDirection: 'row',
    alignItems: 'center',
    gap: space.sm,
    paddingHorizontal: space.md,
    paddingVertical: space.xs,
  },
  dot: { width: 8, height: 8, borderRadius: 4 },
  dotLive: { backgroundColor: theme.success },
  dotIdle: { backgroundColor: theme.textFaint },
  status: { color: theme.body, fontSize: 13, flex: 1 },
  control: {
    minHeight: MIN_TOUCH - 12,
    justifyContent: 'center',
    paddingHorizontal: space.sm,
    borderRadius: 6,
    backgroundColor: theme.surface,
  },
  controlText: { color: theme.body, fontSize: 13, fontWeight: '600' },
  disabled: { opacity: 0.45 },
  leave: { minHeight: MIN_TOUCH - 12, justifyContent: 'center', paddingHorizontal: space.sm },
  leaveText: { color: theme.danger, fontSize: 13, fontWeight: '600' },
  pressed: { opacity: 0.7 },
})
