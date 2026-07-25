import { create } from 'zustand'
import type { Channel } from '../lib/types'

interface ChannelState {
  channels: Channel[]
  activeChannel: Channel | null

  setChannels: (chs: Channel[]) => void
  setActiveChannel: (ch: Channel | null) => void
  /** Insert, or merge in place if the id is already present. */
  addChannel: (ch: Channel) => void
  /** addChannel plus activeChannel sync — for realtime `channel.updated` frames. */
  upsertChannel: (ch: Channel) => void
  removeChannel: (channelId: string) => void
  setUnreadCount: (channelId: string, count: number) => void
  clear: () => void
}

/** Merges into the matching entry (preserving order) or appends. */
function merge(list: Channel[], ch: Channel): Channel[] {
  const idx = list.findIndex((c) => c.id === ch.id)
  if (idx === -1) return [...list, ch]
  const next = list.slice()
  next[idx] = { ...next[idx], ...ch }
  return next
}

export const useChannelStore = create<ChannelState>()((set) => ({
  channels: [],
  activeChannel: null,

  setChannels: (channels) =>
    set((s) => ({
      channels,
      // Keep the open channel's object in sync with the refreshed list, so a
      // rename or an unread reset is not masked by a stale header object.
      activeChannel: s.activeChannel
        ? channels.find((c) => c.id === s.activeChannel!.id) ?? s.activeChannel
        : null,
    })),

  setActiveChannel: (ch) => set({ activeChannel: ch }),

  // Deduped: POST /workspaces/{id}/channels/dm is idempotent server-side and
  // answers 200 with the EXISTING channel, so a blind append put a duplicate
  // row in the sidebar and a duplicate key in the SectionList.
  addChannel: (ch) => set((s) => ({ channels: merge(s.channels, ch) })),

  upsertChannel: (ch) =>
    set((s) => ({
      channels: merge(s.channels, ch),
      activeChannel:
        s.activeChannel && s.activeChannel.id === ch.id ? { ...s.activeChannel, ...ch } : s.activeChannel,
    })),

  removeChannel: (channelId) =>
    set((s) => {
      if (!s.channels.some((c) => c.id === channelId)) {
        return s.activeChannel?.id === channelId ? { activeChannel: null } : s
      }
      return {
        channels: s.channels.filter((c) => c.id !== channelId),
        activeChannel: s.activeChannel?.id === channelId ? null : s.activeChannel,
      }
    }),

  setUnreadCount: (channelId, count) =>
    set((s) => {
      const idx = s.channels.findIndex((c) => c.id === channelId)
      if (idx === -1 || s.channels[idx].unread_count === count) return s
      const next = s.channels.slice()
      next[idx] = { ...next[idx], unread_count: count }
      return { channels: next }
    }),

  clear: () => set({ channels: [], activeChannel: null }),
}))
