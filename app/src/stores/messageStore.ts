import { create } from 'zustand'
import type { Message } from '../lib/types'

const EMPTY: Message[] = []

interface MessageState {
  messages: Record<string, Message[]>
  cursors: Record<string, string>
  hasMore: Record<string, boolean>
  setMessages: (channelId: string, msgs: Message[], cursor: string, hasMore: boolean) => void
  addMessage: (channelId: string, msg: Message) => void
  clearChannel: (channelId: string) => void
}

export const useMessageStore = create<MessageState>()((set) => ({
  messages: {},
  cursors: {},
  hasMore: {},

  setMessages: (channelId, msgs, cursor, hasMore) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: msgs.reverse() },
      cursors: { ...s.cursors, [channelId]: cursor },
      hasMore: { ...s.hasMore, [channelId]: hasMore },
    })),

  addMessage: (channelId, msg) =>
    set((s) => ({
      messages: {
        ...s.messages,
        [channelId]: [...(s.messages[channelId] ?? EMPTY), msg],
      },
    })),

  clearChannel: (channelId) =>
    set((s) => {
      const msgs = { ...s.messages }
      delete msgs[channelId]
      return { messages: msgs }
    }),
}))
