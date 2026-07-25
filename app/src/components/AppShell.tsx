import React, { useEffect, useRef } from 'react'
import { Platform, Pressable, View } from 'react-native'
import { PANE, space, useResponsive } from '../lib/responsive'
import { theme } from '../lib/theme'
import { focusView } from './a11y'

/**
 * The pane container.
 *
 * `WorkspaceScreen` used to return EITHER the channel list OR the conversation,
 * which is the right answer on a phone and the single most visible symptom of a
 * blown-up phone on a monitor: you could never see the list and a conversation
 * at once. This owns that decision instead, per viewport:
 *
 *   wide    sidebar │ conversation │ thread     — three columns, nothing hidden
 *   medium  sidebar │ conversation             — the thread covers the conversation
 *   compact         conversation               — one pane, as before
 *
 * It is deliberately layout-only: every pane arrives as a node, so the shell has
 * no data dependencies, and the same tree is reachable at any width. Dragging a
 * browser window across a breakpoint re-renders through `useResponsive` and
 * reflows without remounting a pane — the panes are the same elements either
 * side of the boundary.
 */

export interface AppShellProps {
  /** Channel list. Mounted at every tier except compact-with-a-channel-open. */
  sidebar: React.ReactNode
  /** The open conversation, or null when nothing is selected. */
  conversation: React.ReactNode | null
  /**
   * Shown in the conversation pane on medium/wide when nothing is selected. On
   * compact there is no such moment: the sidebar is what fills the screen.
   */
  conversationEmpty?: React.ReactNode
  /** The open thread, or null. The shell decides column/overlay/full screen. */
  thread?: React.ReactNode | null
  /** Dismiss the thread — the scrim and Escape both call it. */
  onDismissThread?: () => void
}

/** Frozen so the absolute layers do not hand React a new style identity. */
const FILL = {
  position: 'absolute',
  top: 0,
  left: 0,
  right: 0,
  bottom: 0,
} as const

const FULL_SCREEN_THREAD = { ...FILL, backgroundColor: theme.bg } as const

const SCRIM = { flex: 1, backgroundColor: 'rgba(2,6,23,0.6)' } as const

export default function AppShell({
  sidebar,
  conversation,
  conversationEmpty = null,
  thread = null,
  onDismissThread,
}: AppShellProps) {
  const { twoPane, threePane, threadOverlays, sidebarWidth, threadWidth, width } = useResponsive()

  const conversationRef = useRef<View>(null)
  const threadOpen = !!thread
  const wasOpen = useRef(false)

  /**
   * Return the screen-reader cursor to the conversation when a thread closes.
   *
   * The thread used to be a `Modal`, which restores focus on dismiss for free.
   * An overlay in the same tree does not, so closing one left the cursor on a
   * control that no longer exists.
   */
  useEffect(() => {
    if (threadOpen) {
      wasOpen.current = true
      return
    }
    if (!wasOpen.current) return
    wasOpen.current = false
    focusView(conversationRef)
  }, [threadOpen])

  /**
   * Escape closes the thread. Web only, and only a convenience — the close
   * button is always present — but a pointer user with a keyboard expects a
   * dismissable overlay to answer Escape.
   */
  useEffect(() => {
    if (!threadOpen || !onDismissThread || Platform.OS !== 'web') return
    if (typeof document === 'undefined') return
    const onKeyDown = (e: KeyboardEvent) => {
      if (e.key === 'Escape') onDismissThread()
    }
    document.addEventListener('keydown', onKeyDown)
    return () => document.removeEventListener('keydown', onKeyDown)
  }, [threadOpen, onDismissThread])

  // --- compact: one pane -----------------------------------------------------

  if (!twoPane) {
    // The conversation stays mounted underneath rather than being swapped out,
    // exactly as it was under the thread's Modal: closing a thread must not
    // reload the channel. It is hidden from assistive tech instead, which is
    // the part a Modal did for free.
    return (
      <View style={{ flex: 1, backgroundColor: theme.bg }}>
        <View
          ref={conversationRef}
          style={{ flex: 1 }}
          accessibilityElementsHidden={threadOpen}
          importantForAccessibility={threadOpen ? 'no-hide-descendants' : 'auto'}
        >
          {conversation ?? sidebar}
        </View>
        {thread ? (
          <View accessibilityViewIsModal style={FULL_SCREEN_THREAD}>
            {thread}
          </View>
        ) : null}
      </View>
    )
  }

  // --- medium / wide: panes --------------------------------------------------

  // The thread overlays the conversation only, so it is a child of that pane.
  // That also keeps DOM order — and therefore Tab order — sidebar, conversation,
  // thread, and it is why the overlay is NOT marked `accessibilityViewIsModal`:
  // the sidebar beside it stays live and reachable.
  const overlayWidth = Math.max(280, Math.min(PANE.thread, width - sidebarWidth - space.xxxl))

  return (
    <View style={{ flex: 1, flexDirection: 'row', backgroundColor: theme.bg }}>
      <View
        style={{
          width: sidebarWidth,
          backgroundColor: theme.surface,
          borderRightWidth: 1,
          borderRightColor: theme.border,
        }}
      >
        {sidebar}
      </View>

      <View ref={conversationRef} style={{ flex: 1, minWidth: 0 }}>
        {conversation ?? conversationEmpty}

        {threadOverlays && thread ? (
          <View style={{ ...FILL, flexDirection: 'row' }}>
            <Pressable
              onPress={onDismissThread}
              accessibilityRole="button"
              accessibilityLabel="Close thread"
              style={SCRIM}
            />
            <View
              style={{
                width: overlayWidth,
                backgroundColor: theme.bg,
                borderLeftWidth: 1,
                borderLeftColor: theme.borderStrong,
              }}
            >
              {thread}
            </View>
          </View>
        ) : null}
      </View>

      {threePane && thread ? (
        <View
          style={{
            width: threadWidth,
            borderLeftWidth: 1,
            borderLeftColor: theme.border,
          }}
        >
          {thread}
        </View>
      ) : null}
    </View>
  )
}
