// Centralized dark theme tokens (Slate / Indigo). Mirrors the colors that were
// previously inlined across components.
export const theme = {
  bg: '#020617',        // app background
  surface: '#0f172a',   // cards / inputs
  surfaceAlt: '#1e293b',
  border: '#1e293b',
  borderStrong: '#334155',
  text: '#ffffff',
  textMuted: '#94a3b8',
  textFaint: '#64748b',
  textDim: '#475569',
  body: '#e2e8f0',
  primary: '#4f46e5',
  primaryText: '#ffffff',
  accent: '#818cf8',
  danger: '#ef4444',
  success: '#22c55e',
  warning: '#f59e0b',
} as const

// Deterministic avatar color from a user id (matches MessageItem's original hash).
export function avatarColor(userId: string): string {
  const hue = userId.split('').reduce((a, c) => a + c.charCodeAt(0), 0) % 360
  return `hsl(${hue}, 60%, 50%)`
}

export const presenceColor: Record<string, string> = {
  online: '#22c55e',
  away: '#f59e0b',
  dnd: '#ef4444',
  offline: '#475569',
}
