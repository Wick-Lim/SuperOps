import { useRef, useEffect } from 'react'
import type { Message } from '@/lib/types'
import MessageItem from './MessageItem'

interface Props {
  messages: Message[]
  hasMore: boolean
  onLoadMore: () => void
}

export default function MessageList({ messages, hasMore, onLoadMore }: Props) {
  const bottomRef = useRef<HTMLDivElement>(null)
  const prevLength = useRef(0)

  useEffect(() => {
    // Auto-scroll on new messages
    if (messages.length > prevLength.current) {
      bottomRef.current?.scrollIntoView({ behavior: 'smooth' })
    }
    prevLength.current = messages.length
  }, [messages.length])

  return (
    <div className="flex-1 overflow-y-auto px-5 py-4">
      {hasMore && (
        <div className="text-center mb-4">
          <button
            onClick={onLoadMore}
            className="text-sm text-brand-400 hover:text-brand-300 transition-colors"
          >
            Load more messages
          </button>
        </div>
      )}

      {messages.length === 0 && (
        <div className="flex items-center justify-center h-full text-surface-200/50">
          <p>No messages yet. Start the conversation!</p>
        </div>
      )}

      <div className="space-y-1">
        {messages.map((msg, i) => {
          const prev = messages[i - 1]
          const showHeader = !prev || prev.user_id !== msg.user_id ||
            new Date(msg.created_at).getTime() - new Date(prev.created_at).getTime() > 300000
          return <MessageItem key={msg.id} message={msg} showHeader={showHeader} />
        })}
      </div>

      <div ref={bottomRef} />
    </div>
  )
}
