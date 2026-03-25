import { create } from 'zustand'
import type { Message } from '@/lib/types'

interface MessageState {
  messages: Record<string, Message[]> // channelId -> messages
  cursors: Record<string, string>
  hasMore: Record<string, boolean>
  setMessages: (channelId: string, msgs: Message[], cursor: string, hasMore: boolean) => void
  appendMessages: (channelId: string, msgs: Message[], cursor: string, hasMore: boolean) => void
  addMessage: (channelId: string, msg: Message) => void
  updateMessage: (channelId: string, msg: Message) => void
  removeMessage: (channelId: string, messageId: string) => void
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

  appendMessages: (channelId, msgs, cursor, hasMore) =>
    set((s) => ({
      messages: {
        ...s.messages,
        [channelId]: [...msgs.reverse(), ...(s.messages[channelId] || [])],
      },
      cursors: { ...s.cursors, [channelId]: cursor },
      hasMore: { ...s.hasMore, [channelId]: hasMore },
    })),

  addMessage: (channelId, msg) =>
    set((s) => ({
      messages: {
        ...s.messages,
        [channelId]: [...(s.messages[channelId] || []), msg],
      },
    })),

  updateMessage: (channelId, msg) =>
    set((s) => ({
      messages: {
        ...s.messages,
        [channelId]: (s.messages[channelId] || []).map((m) =>
          m.id === msg.id ? msg : m
        ),
      },
    })),

  removeMessage: (channelId, messageId) =>
    set((s) => ({
      messages: {
        ...s.messages,
        [channelId]: (s.messages[channelId] || []).filter((m) => m.id !== messageId),
      },
    })),

  clearChannel: (channelId) =>
    set((s) => {
      const msgs = { ...s.messages }
      delete msgs[channelId]
      return { messages: msgs }
    }),
}))
