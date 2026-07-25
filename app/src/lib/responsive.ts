import { useWindowDimensions } from 'react-native'

/**
 * Breakpoints and the layout decisions that hang off them.
 *
 * The app was built phone-first and shipped to the web unchanged, so a 1600px
 * browser got a single full-width column and had to leave the conversation to
 * see the channel list. Everything here exists to describe how much room is
 * available, and what to do with it.
 *
 * Tiers are named for the LAYOUT they produce, not for a device. "tablet" is a
 * guess about hardware; "medium" is a fact about the viewport, and a half-width
 * desktop window gets the right answer for free.
 */

export const BREAKPOINTS = {
  /** One pane at a time. Below this a second column cannot hold a message. */
  medium: 768,
  /** Room for a third pane without squeezing the conversation under ~560px. */
  wide: 1280,
} as const

export type Tier = 'compact' | 'medium' | 'wide'

/**
 * Fixed pane widths. The conversation takes the remainder, which is what should
 * absorb a resize — the sidebar and thread have a natural size and get worse,
 * not better, when stretched.
 */
export const PANE = {
  /** Fits "# a-long-channel-name" plus an unread badge without truncating. */
  sidebar: 260,
  /** Narrower on medium, where it competes directly with the conversation. */
  sidebarMedium: 220,
  /** A reply plus its composer stay readable; below ~340 they do not. */
  thread: 380,
  /** Never let the conversation collapse to a gutter. */
  minConversation: 480,
} as const

/**
 * Reading measure for centred, single-column screens (settings, admin forms).
 * Long lines are the other failure mode of a full-width phone layout on a
 * monitor: text that runs the width of the screen is genuinely hard to read.
 */
export const CONTENT_MAX_WIDTH = 760

/** 4px base. Replaces the magic numbers that were inlined per component. */
export const space = {
  xs: 4,
  sm: 8,
  md: 12,
  lg: 16,
  xl: 24,
  xxl: 32,
  xxxl: 40,
} as const

/**
 * Minimum interactive size.
 *
 * 44px is the TOUCH guideline, and it is right on a phone. Applying it to a
 * mouse-driven desktop list is what makes this app read as a blown-up phone:
 * rows are sparse, and fewer messages fit on a screen that has more room than
 * the phone ever did. Pointer input is precise, so wide tiers tighten to 36 —
 * still comfortably clickable, and still large enough for a touchscreen laptop.
 *
 * This is a density decision, not an accessibility one: WCAG's target-size
 * guidance is about touch, and keyboard/AT users are unaffected by the change.
 */
export const MIN_TOUCH = 44
export const MIN_POINTER = 36

export type Responsive = {
  tier: Tier
  width: number
  height: number
  /** Sidebar and conversation are both on screen. */
  twoPane: boolean
  /** Thread gets its own column instead of covering the conversation. */
  threePane: boolean
  /** Thread must overlay because there is no room beside the conversation. */
  threadOverlays: boolean
  /** Pixels available to the sidebar, 0 when it is a separate screen. */
  sidebarWidth: number
  /** Pixels available to the thread pane, 0 when it overlays or is a screen. */
  threadWidth: number
  /** Minimum height for rows, buttons and inputs at this tier. */
  minTouch: number
  /** Horizontal page padding. Phones need edge-to-edge; desktops need air. */
  gutter: number
}

export function tierFor(width: number): Tier {
  if (width >= BREAKPOINTS.wide) return 'wide'
  if (width >= BREAKPOINTS.medium) return 'medium'
  return 'compact'
}

/**
 * The single source of layout truth for a render.
 *
 * `useWindowDimensions` re-renders on resize and on orientation change, so a
 * browser window dragged across a breakpoint reflows without a reload.
 */
export function useResponsive(): Responsive {
  const { width, height } = useWindowDimensions()
  const tier = tierFor(width)

  const sidebarWidth = tier === 'wide' ? PANE.sidebar : tier === 'medium' ? PANE.sidebarMedium : 0

  // Only promote the thread to its own column when the conversation keeps a
  // usable width after the split. A hard breakpoint alone would give a 1280px
  // window a 640px conversation, but a 1280px window with a wide sidebar and a
  // 380px thread leaves it at 640 — fine — while a future wider sidebar would
  // not. Deriving it from the arithmetic keeps that honest.
  const threePane = tier === 'wide' && width - sidebarWidth - PANE.thread >= PANE.minConversation

  return {
    tier,
    width,
    height,
    twoPane: tier !== 'compact',
    threePane,
    threadOverlays: tier !== 'compact' && !threePane,
    sidebarWidth,
    threadWidth: threePane ? PANE.thread : 0,
    minTouch: tier === 'compact' ? MIN_TOUCH : MIN_POINTER,
    gutter: tier === 'compact' ? space.lg : space.xl,
  }
}

/**
 * Picks a value per tier, falling back down to the nearest defined one, so a
 * caller can specify only where the value actually changes.
 */
export function byTier<T>(tier: Tier, values: { compact: T; medium?: T; wide?: T }): T {
  if (tier === 'wide') return values.wide ?? values.medium ?? values.compact
  if (tier === 'medium') return values.medium ?? values.compact
  return values.compact
}
