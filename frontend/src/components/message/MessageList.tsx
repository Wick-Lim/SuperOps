import { useRef, useCallback } from 'react'
import { Virtuoso, type VirtuosoHandle } from 'react-virtuoso'
import type { Message } from '@/lib/types'
import MessageItem from './MessageItem'

interface Props {
  messages: Message[]
  hasMore: boolean
  onLoadMore: () => void
}

export default function MessageList({ messages, hasMore, onLoadMore }: Props) {
  const virtuosoRef = useRef<VirtuosoHandle>(null)

  const itemContent = useCallback((index: number) => {
    const msg = messages[index]
    const prev = index > 0 ? messages[index - 1] : null
    const showHeader = !prev || prev.user_id !== msg.user_id ||
      new Date(msg.created_at).getTime() - new Date(prev.created_at).getTime() > 300000
    return <MessageItem key={msg.id} message={msg} showHeader={showHeader} />
  }, [messages])

  if (messages.length === 0) {
    return (
      <div className="flex-1 flex items-center justify-center text-surface-200/50">
        <p>No messages yet. Start the conversation!</p>
      </div>
    )
  }

  return (
    <Virtuoso
      ref={virtuosoRef}
      className="flex-1"
      style={{ height: '100%' }}
      data={messages}
      totalCount={messages.length}
      itemContent={itemContent}
      followOutput="smooth"
      initialTopMostItemIndex={messages.length - 1}
      atTopStateChange={(atTop) => { if (atTop && hasMore) onLoadMore() }}
      components={{
        Header: () =>
          hasMore ? (
            <div className="text-center py-3">
              <button onClick={onLoadMore} className="text-sm text-brand-400 hover:text-brand-300">
                Load more messages
              </button>
            </div>
          ) : null,
      }}
    />
  )
}
