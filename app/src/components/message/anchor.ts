import type { GestureResponderEvent } from 'react-native'

/**
 * Where a popover should hang off, in window coordinates.
 *
 * The emoji picker is a bottom sheet, which is the right idiom for a thumb: the
 * sheet is close to the hand and the target that opened it is a finger-width
 * away. With a pointer on a 1000px-tall window the same sheet slides up from the
 * far edge of the screen, hundreds of pixels from the ＋ that was clicked, and
 * the connection between the two is lost. Capturing the press position lets the
 * picker open next to the thing it acts on instead.
 *
 * `undefined` is a valid answer — a press with no coordinates (a screen reader
 * activation, a hardware keyboard) falls back to the sheet.
 */
export interface Anchor {
  x: number
  y: number
}

export function anchorFrom(e: GestureResponderEvent | undefined): Anchor | undefined {
  const t = e?.nativeEvent
  if (!t || typeof t.pageX !== 'number' || typeof t.pageY !== 'number') return undefined
  // A keyboard/AT activation reports (0,0) on web; that is not a pointer.
  if (t.pageX === 0 && t.pageY === 0) return undefined
  return { x: t.pageX, y: t.pageY }
}

/** Keeps `value` inside `[min, max]`, tolerating an inverted range. */
export function clamp(value: number, min: number, max: number): number {
  if (max < min) return min
  return Math.min(Math.max(value, min), max)
}
