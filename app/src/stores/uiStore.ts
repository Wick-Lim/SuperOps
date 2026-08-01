import { create } from 'zustand'
import type { Message, PresenceStatus } from '../lib/types'

/**
 * Realtime connection state, mirrored out of `WebSocketManager` so any component
 * can select it. Without this the user is shown stale data with no indication
 * that realtime is down.
 */
export type ConnectionStatus = 'idle' | 'connecting' | 'connected' | 'reconnecting' | 'offline'

const NO_TYPING: string[] = []

interface UiState {
  // userId -> presence status
  presence: Record<string, PresenceStatus>
  // channelId -> userIds currently typing (expiry timers live in WebSocketManager)
  typing: Record<string, string[]>
  unreadNotifications: number

  connection: ConnectionStatus
  /** Last WS `error` frame code, cleared on the next successful connect. */
  connectionError: string | null

  /**
   * The thread currently open, if any.
   *
   * This lives in the store rather than inside ChannelView because the layout
   * that owns it depends on the viewport: a wide window renders it as a third
   * pane beside the conversation, a medium one overlays it, and a phone shows
   * it full screen. Only the shell knows which, so the shell has to be able to
   * see that a thread is open — a `useState` inside ChannelView cannot tell it.
   */
  activeThread: { channelId: string; parent: Message } | null

  setPresence: (map: Record<string, PresenceStatus>) => void
  setUserPresence: (userId: string, status: PresenceStatus) => void
  setTyping: (channelId: string, userIds: string[]) => void
  addTyping: (channelId: string, userId: string) => void
  removeTyping: (channelId: string, userId: string) => void
  clearTyping: (channelId?: string) => void
  setUnread: (n: number) => void
  setConnection: (status: ConnectionStatus, error?: string | null) => void
  openThread: (channelId: string, parent: Message) => void
  closeThread: () => void
  clear: () => void
}

export const useUiStore = create<UiState>()((set) => ({
  presence: {},
  typing: {},
  unreadNotifications: 0,
  connection: 'idle',
  connectionError: null,
  activeThread: null,

  setPresence: (map) => set({ presence: map }),
  setUserPresence: (userId, status) =>
    set((s) => (s.presence[userId] === status ? s : { presence: { ...s.presence, [userId]: status } })),

  setTyping: (channelId, userIds) =>
    set((s) => ({ typing: { ...s.typing, [channelId]: userIds } })),

  addTyping: (channelId, userId) =>
    set((s) => {
      const cur = s.typing[channelId] ?? NO_TYPING
      if (cur.includes(userId)) return s
      return { typing: { ...s.typing, [channelId]: [...cur, userId] } }
    }),

  removeTyping: (channelId, userId) =>
    set((s) => {
      const cur = s.typing[channelId]
      if (!cur || !cur.includes(userId)) return s
      return { typing: { ...s.typing, [channelId]: cur.filter((u) => u !== userId) } }
    }),

  clearTyping: (channelId) =>
    set((s) => {
      if (!channelId) return Object.keys(s.typing).length === 0 ? s : { typing: {} }
      if (!s.typing[channelId]) return s
      const typing = { ...s.typing }
      delete typing[channelId]
      return { typing }
    }),

  setUnread: (n) => set((s) => (s.unreadNotifications === n ? s : { unreadNotifications: n })),

  setConnection: (status, error = null) =>
    set((s) =>
      s.connection === status && s.connectionError === error ? s : { connection: status, connectionError: error },
    ),

  openThread: (channelId, parent) =>
    set((s) =>
      s.activeThread?.parent.id === parent.id ? s : { activeThread: { channelId, parent } },
    ),

  closeThread: () => set((s) => (s.activeThread === null ? s : { activeThread: null })),

  clear: () =>
    set({
      presence: {},
      typing: {},
      unreadNotifications: 0,
      connection: 'idle',
      connectionError: null,
      activeThread: null,
    }),
}))
