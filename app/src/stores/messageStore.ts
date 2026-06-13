import { create } from 'zustand'
import type { Message, Reaction } from '../lib/types'

const EMPTY: Message[] = []

interface MessageState {
  messages: Record<string, Message[]>
  cursors: Record<string, string>
  hasMore: Record<string, boolean>
  setMessages: (channelId: string, msgs: Message[], cursor: string, hasMore: boolean) => void
  prependMessages: (channelId: string, older: Message[], cursor: string, hasMore: boolean) => void
  addMessage: (channelId: string, msg: Message) => void
  updateMessage: (channelId: string, msg: Message) => void
  removeMessage: (channelId: string, messageId: string) => void
  applyReaction: (channelId: string, r: Reaction, added: boolean) => void
  clearChannel: (channelId: string) => void
}

function upsert(list: Message[], msg: Message): Message[] {
  const idx = list.findIndex((m) => m.id === msg.id)
  if (idx === -1) return [...list, msg]
  const next = list.slice()
  next[idx] = msg
  return next
}

export const useMessageStore = create<MessageState>()((set) => ({
  messages: {},
  cursors: {},
  hasMore: {},

  setMessages: (channelId, msgs, cursor, hasMore) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: msgs.slice().reverse() },
      cursors: { ...s.cursors, [channelId]: cursor },
      hasMore: { ...s.hasMore, [channelId]: hasMore },
    })),

  // older messages (a further page back) go at the front of the ascending list
  prependMessages: (channelId, older, cursor, hasMore) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: [...older.slice().reverse(), ...(s.messages[channelId] ?? EMPTY)] },
      cursors: { ...s.cursors, [channelId]: cursor },
      hasMore: { ...s.hasMore, [channelId]: hasMore },
    })),

  addMessage: (channelId, msg) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: upsert(s.messages[channelId] ?? EMPTY, msg) },
    })),

  updateMessage: (channelId, msg) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: upsert(s.messages[channelId] ?? EMPTY, msg) },
    })),

  removeMessage: (channelId, messageId) =>
    set((s) => ({
      messages: { ...s.messages, [channelId]: (s.messages[channelId] ?? EMPTY).filter((m) => m.id !== messageId) },
    })),

  applyReaction: (channelId, r, added) =>
    set((s) => {
      const list = s.messages[channelId] ?? EMPTY
      const idx = list.findIndex((m) => m.id === r.message_id)
      if (idx === -1) return s
      const m = list[idx]
      const reactions = (m.reactions ?? []).filter(
        (x) => !(x.user_id === r.user_id && x.emoji === r.emoji),
      )
      if (added) reactions.push(r)
      const next = list.slice()
      next[idx] = { ...m, reactions }
      return { messages: { ...s.messages, [channelId]: next } }
    }),

  clearChannel: (channelId) =>
    set((s) => {
      const msgs = { ...s.messages }
      delete msgs[channelId]
      return { messages: msgs }
    }),
}))
