import { create } from 'zustand'

interface PresenceState {
  presence: Record<string, string> // userId -> 'online' | 'away' | 'dnd' | 'offline'
  typingUsers: Record<string, string[]> // channelId -> userIds
  setPresence: (userId: string, status: string) => void
  setBulkPresence: (map: Record<string, string>) => void
  setTyping: (channelId: string, userId: string) => void
  clearTyping: (channelId: string, userId: string) => void
}

export const usePresenceStore = create<PresenceState>()((set) => ({
  presence: {},
  typingUsers: {},

  setPresence: (userId, status) =>
    set((s) => ({ presence: { ...s.presence, [userId]: status } })),

  setBulkPresence: (map) =>
    set((s) => ({ presence: { ...s.presence, ...map } })),

  setTyping: (channelId, userId) =>
    set((s) => {
      const current = s.typingUsers[channelId] || []
      if (current.includes(userId)) return s
      return { typingUsers: { ...s.typingUsers, [channelId]: [...current, userId] } }
    }),

  clearTyping: (channelId, userId) =>
    set((s) => {
      const current = s.typingUsers[channelId] || []
      return { typingUsers: { ...s.typingUsers, [channelId]: current.filter((u) => u !== userId) } }
    }),
}))
