import { create } from 'zustand'
import type { Message, Reaction } from '../lib/types'

const EMPTY: Message[] = []

/**
 * Hard cap on the live (appended) window per channel. A long-lived session in a
 * busy channel otherwise grows without bound.
 *
 * When the cap trims the oldest messages the pagination cursor is invalidated
 * (`cursors[channelId] = ''`) rather than kept: the retained cursor points past
 * the messages that were just dropped, so paging with it would splice a hole
 * into the history. Cursors are opaque server tokens, so the client cannot mint
 * a replacement — reopening the channel refetches from the top.
 */
const MAX_LIVE_MESSAGES = 500

interface MessageState {
  messages: Record<string, Message[]>
  cursors: Record<string, string>
  hasMore: Record<string, boolean>

  setMessages: (channelId: string, msgs: Message[], cursor: string, hasMore: boolean) => void
  prependMessages: (channelId: string, older: Message[], cursor: string, hasMore: boolean) => void
  addMessage: (channelId: string, msg: Message) => void
  /** Replace-only: a message outside the loaded window is ignored, not appended. */
  updateMessage: (channelId: string, msg: Message) => void
  removeMessage: (channelId: string, messageId: string) => void
  applyReaction: (channelId: string, r: Reaction, added: boolean) => void
  /** Drops the messages AND the pagination state for one channel. */
  clearChannel: (channelId: string) => void
  /** Keeps only the newest `keep` messages; use when a channel loses focus. */
  trimChannel: (channelId: string, keep?: number) => void
  clear: () => void
}

function replace(list: Message[], msg: Message): Message[] | null {
  const idx = list.findIndex((m) => m.id === msg.id)
  if (idx === -1) return null
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
    set((s) => {
      const list = s.messages[channelId] ?? EMPTY
      const replaced = replace(list, msg)
      if (replaced) return { messages: { ...s.messages, [channelId]: replaced } }

      const appended = [...list, msg]
      if (appended.length <= MAX_LIVE_MESSAGES) {
        return { messages: { ...s.messages, [channelId]: appended } }
      }
      return {
        messages: { ...s.messages, [channelId]: appended.slice(appended.length - MAX_LIVE_MESSAGES) },
        cursors: { ...s.cursors, [channelId]: '' },
        hasMore: { ...s.hasMore, [channelId]: true },
      }
    }),

  // Replace-only. Upserting here meant a `message.updated` frame for a message
  // scrolled out of the loaded window APPENDED it, so an old edited message
  // jumped to the bottom of the channel as if it had just been sent.
  updateMessage: (channelId, msg) =>
    set((s) => {
      const replaced = replace(s.messages[channelId] ?? EMPTY, msg)
      if (!replaced) return s
      return { messages: { ...s.messages, [channelId]: replaced } }
    }),

  removeMessage: (channelId, messageId) =>
    set((s) => {
      const list = s.messages[channelId]
      if (!list || !list.some((m) => m.id === messageId)) return s
      return { messages: { ...s.messages, [channelId]: list.filter((m) => m.id !== messageId) } }
    }),

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

  // Deleting only `messages` leaked the `cursors` and `hasMore` entries, so a
  // reopened channel started with a stale cursor pointing into a window that
  // was no longer loaded.
  clearChannel: (channelId) =>
    set((s) => {
      const messages = { ...s.messages }
      const cursors = { ...s.cursors }
      const hasMore = { ...s.hasMore }
      delete messages[channelId]
      delete cursors[channelId]
      delete hasMore[channelId]
      return { messages, cursors, hasMore }
    }),

  trimChannel: (channelId, keep = 100) =>
    set((s) => {
      const list = s.messages[channelId]
      if (!list || list.length <= keep) return s
      return {
        messages: { ...s.messages, [channelId]: list.slice(list.length - keep) },
        cursors: { ...s.cursors, [channelId]: '' },
        hasMore: { ...s.hasMore, [channelId]: true },
      }
    }),

  clear: () => set({ messages: {}, cursors: {}, hasMore: {} }),
}))
