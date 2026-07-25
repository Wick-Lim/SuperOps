import { useCallback, useEffect, useRef } from 'react'
import type React from 'react'
import { AccessibilityInfo, View, findNodeHandle } from 'react-native'

/**
 * Minimum touch target. WCAG 2.5.5 (AAA) and both platform HIGs land on 44dp;
 * every icon-only control in this app is smaller than that, so controls either
 * size to this or make up the difference with `hitSlop`.
 */
export const MIN_TOUCH = 44

/** `hitSlop` that grows a `size`×`size` control to MIN_TOUCH in both axes. */
export function touchSlop(size: number): number {
  return Math.max(0, Math.round((MIN_TOUCH - size) / 2))
}

/**
 * Moves the screen reader cursor onto `ref`.
 *
 * A `Modal` traps native focus but NOT accessibility focus: VoiceOver/TalkBack
 * stay on whatever was focused behind it until something moves them, so a sheet
 * that opens is silent and its first control is unreachable by swipe on iOS.
 */
export function focusView(ref: React.RefObject<unknown>): void {
  const target = ref.current as Parameters<typeof findNodeHandle>[0]
  if (!target) return
  const node = findNodeHandle(target)
  if (node != null) AccessibilityInfo.setAccessibilityFocus(node)
}

/**
 * Moves screen-reader focus into a modal the first time it lays out.
 *
 * Spread the result onto the element that should be read first:
 * `<View {...useModalFocus(visible)} accessible accessibilityRole="header">`.
 * Focusing on every `onLayout` instead would yank the cursor back to the title
 * whenever the keyboard opened over a sheet.
 */
export function useModalFocus(visible: boolean): {
  ref: React.RefObject<View | null>
  onLayout: () => void
} {
  const ref = useRef<View>(null)
  const focused = useRef(false)

  useEffect(() => {
    if (!visible) focused.current = false
  }, [visible])

  const onLayout = useCallback(() => {
    if (focused.current) return
    focused.current = true
    focusView(ref)
  }, [])

  return { ref, onLayout }
}

/**
 * Speaks `message` immediately.
 *
 * `accessibilityLiveRegion` is Android-only, so incoming messages are announced
 * with both: the live region for TalkBack, this for VoiceOver.
 */
export function announce(message: string): void {
  if (!message) return
  AccessibilityInfo.announceForAccessibility(message)
}
