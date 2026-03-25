import { create } from 'zustand'
import type { Message } from '@/lib/types'

interface ThreadState {
  activeThreadId: string | null
  parentMessage: Message | null
  replies: Message[]
  openThread: (parentMessage: Message) => void
  closeThread: () => void
  setReplies: (replies: Message[]) => void
  addReply: (reply: Message) => void
}

export const useThreadStore = create<ThreadState>()((set) => ({
  activeThreadId: null,
  parentMessage: null,
  replies: [],
  openThread: (msg) => set({ activeThreadId: msg.id, parentMessage: msg, replies: [] }),
  closeThread: () => set({ activeThreadId: null, parentMessage: null, replies: [] }),
  setReplies: (replies) => set({ replies }),
  addReply: (reply) => set((s) => ({ replies: [...s.replies, reply] })),
}))
