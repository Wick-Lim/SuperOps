/**
 * The breakpoint contract.
 *
 * Every pane decision in the app derives from these functions, so a regression
 * here silently changes the layout at one width and nowhere else — the kind of
 * thing that only shows up when someone opens the app on a laptop they happen
 * to own. Asserting the arithmetic directly is also the only verification
 * available for the medium and compact tiers: the automation environment's
 * window manager refuses to resize the browser below its current size, so a
 * screenshot can confirm the wide tier and nothing else.
 */
import { describe, expect, it } from 'vitest'
import { BREAKPOINTS, PANE, tierFor } from '../src/lib/responsive'

/**
 * Mirrors the derivation inside `useResponsive` for a given width. The hook
 * itself only adds `useWindowDimensions`, which is React Native's and not ours
 * to test.
 */
function layoutFor(width: number) {
  const tier = tierFor(width)
  const sidebarWidth = tier === 'wide' ? PANE.sidebar : tier === 'medium' ? PANE.sidebarMedium : 0
  const threePane = tier === 'wide' && width - sidebarWidth - PANE.thread >= PANE.minConversation
  return {
    tier,
    sidebarWidth,
    threePane,
    twoPane: tier !== 'compact',
    threadOverlays: tier !== 'compact' && !threePane,
    conversationWidth: width - sidebarWidth - (threePane ? PANE.thread : 0),
  }
}

describe('tierFor', () => {
  it('places each boundary on the documented side', () => {
    expect(tierFor(BREAKPOINTS.medium - 1)).toBe('compact')
    expect(tierFor(BREAKPOINTS.medium)).toBe('medium')
    expect(tierFor(BREAKPOINTS.wide - 1)).toBe('medium')
    expect(tierFor(BREAKPOINTS.wide)).toBe('wide')
  })

  it('handles degenerate widths without throwing', () => {
    expect(tierFor(0)).toBe('compact')
    expect(tierFor(10_000)).toBe('wide')
  })
})

describe('pane layout', () => {
  it('shows one pane on a phone', () => {
    const l = layoutFor(390) // iPhone-class
    expect(l.twoPane).toBe(false)
    expect(l.threePane).toBe(false)
    expect(l.sidebarWidth).toBe(0)
  })

  it('shows sidebar + conversation on a tablet, with the thread overlaying', () => {
    const l = layoutFor(1024) // iPad landscape
    expect(l.twoPane).toBe(true)
    expect(l.threePane).toBe(false)
    expect(l.threadOverlays).toBe(true)
    expect(l.sidebarWidth).toBe(PANE.sidebarMedium)
  })

  it('shows all three panes on a desktop', () => {
    const l = layoutFor(1600)
    expect(l.threePane).toBe(true)
    expect(l.threadOverlays).toBe(false)
    expect(l.sidebarWidth).toBe(PANE.sidebar)
  })

  /**
   * The reason threePane is arithmetic rather than `tier === 'wide'`. At exactly
   * the wide breakpoint a third column would leave the conversation at
   * 1280 - 260 - 380 = 640, which is fine — but the guard is what keeps that
   * true if the sidebar or thread ever widen.
   */
  it('never splits the conversation below its minimum', () => {
    for (let w = BREAKPOINTS.wide; w <= 2560; w += 7) {
      const l = layoutFor(w)
      if (l.threePane) {
        expect(l.conversationWidth).toBeGreaterThanOrEqual(PANE.minConversation)
      }
    }
  })

  it('gives the conversation every pixel the fixed panes do not take', () => {
    const l = layoutFor(1600)
    expect(l.conversationWidth).toBe(1600 - PANE.sidebar - PANE.thread)
  })

  /** A resize must never produce a tier with no layout at all. */
  it('produces a coherent layout at every width from 320 to 2560', () => {
    for (let w = 320; w <= 2560; w += 1) {
      const l = layoutFor(w)
      expect(['compact', 'medium', 'wide']).toContain(l.tier)
      expect(l.conversationWidth).toBeGreaterThan(0)
      // The thread is either a column or an overlay, never both and never
      // neither once the sidebar is persistent.
      if (l.twoPane) expect(l.threePane).toBe(!l.threadOverlays)
    }
  })
})
