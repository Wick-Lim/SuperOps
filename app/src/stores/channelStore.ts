import { create } from 'zustand'
import type { Channel } from '../lib/types'

interface ChannelState {
  channels: Channel[]
  activeChannel: Channel | null
  setChannels: (chs: Channel[]) => void
  setActiveChannel: (ch: Channel | null) => void
  addChannel: (ch: Channel) => void
}

export const useChannelStore = create<ChannelState>()((set) => ({
  channels: [],
  activeChannel: null,
  setChannels: (channels) => set({ channels }),
  setActiveChannel: (ch) => set({ activeChannel: ch }),
  addChannel: (ch) => set((s) => ({ channels: [...s.channels, ch] })),
}))
