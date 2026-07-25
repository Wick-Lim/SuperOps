/**
 * Message timestamp formatting.
 *
 * `formatTime` used to be the only formatter in the list, so a message from last
 * week rendered as a bare "09:14" with nothing to date it. Rows are now grouped
 * by local calendar day (`dayKey`) and each group is introduced by
 * `formatDayLabel`.
 */

const DAY_MS = 86_400_000

function parse(iso: string): Date | null {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? null : d
}

function startOfLocalDay(d: Date): number {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate()).getTime()
}

export function formatTime(iso: string): string {
  const d = parse(iso)
  return d ? d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' }) : ''
}

/** Local calendar day identity. Equal keys mean "no separator between these". */
export function dayKey(iso: string): string {
  const d = parse(iso)
  return d ? `${d.getFullYear()}-${d.getMonth()}-${d.getDate()}` : ''
}

export function formatDayLabel(iso: string): string {
  const d = parse(iso)
  if (!d) return ''
  const now = new Date()
  const days = Math.round((startOfLocalDay(now) - startOfLocalDay(d)) / DAY_MS)
  if (days === 0) return 'Today'
  if (days === 1) return 'Yesterday'
  return d.toLocaleDateString(
    [],
    d.getFullYear() === now.getFullYear()
      ? { weekday: 'short', month: 'short', day: 'numeric' }
      : { year: 'numeric', month: 'short', day: 'numeric' },
  )
}

/** Unabbreviated form for screen readers, which get no visual day separator. */
export function formatFull(iso: string): string {
  const d = parse(iso)
  if (!d) return ''
  return `${formatDayLabel(iso)} at ${formatTime(iso)}`
}
