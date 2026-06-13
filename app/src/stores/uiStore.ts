import { create } from 'zustand'
import type { PresenceStatus } from '../lib/types'

interface UiState {
  // userId -> presence status
  presence: Record<string, PresenceStatus>
  // channelId -> set of userIds currently typing (with expiry timers managed by caller)
  typing: Record<string, string[]>
  unreadNotifications: number

  setPresence: (map: Record<string, PresenceStatus>) => void
  setUserPresence: (userId: string, status: PresenceStatus) => void
  setTyping: (channelId: string, userIds: string[]) => void
  addTyping: (channelId: string, userId: string) => void
  removeTyping: (channelId: string, userId: string) => void
  setUnread: (n: number) => void
}

export const useUiStore = create<UiState>()((set) => ({
  presence: {},
  typing: {},
  unreadNotifications: 0,

  setPresence: (map) => set({ presence: map }),
  setUserPresence: (userId, status) =>
    set((s) => ({ presence: { ...s.presence, [userId]: status } })),

  setTyping: (channelId, userIds) =>
    set((s) => ({ typing: { ...s.typing, [channelId]: userIds } })),

  addTyping: (channelId, userId) =>
    set((s) => {
      const cur = s.typing[channelId] ?? []
      if (cur.includes(userId)) return s
      return { typing: { ...s.typing, [channelId]: [...cur, userId] } }
    }),

  removeTyping: (channelId, userId) =>
    set((s) => ({
      typing: { ...s.typing, [channelId]: (s.typing[channelId] ?? []).filter((u) => u !== userId) },
    })),

  setUnread: (n) => set({ unreadNotifications: n }),
}))
