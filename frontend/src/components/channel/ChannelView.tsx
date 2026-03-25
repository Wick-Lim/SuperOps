import { useEffect, useCallback } from 'react'
import { useParams } from 'react-router-dom'
import { messageApi } from '@/api/messages'
import { useChannelStore } from '@/stores/channelStore'
import { useMessageStore } from '@/stores/messageStore'
import MessageList from '@/components/message/MessageList'
import MessageInput from '@/components/message/MessageInput'

export default function ChannelView() {
  const { channelId } = useParams<{ channelId: string }>()
  const activeChannel = useChannelStore((s) => s.channels.find((c) => c.id === channelId))
  const { messages, setMessages, hasMore, cursors } = useMessageStore()

  const currentMessages = channelId ? messages[channelId] || [] : []
  const currentCursor = channelId ? cursors[channelId] : undefined
  const currentHasMore = channelId ? hasMore[channelId] ?? true : false

  const loadMessages = useCallback(async () => {
    if (!channelId) return
    try {
      const res = await messageApi.list(channelId)
      setMessages(channelId, res.data, res.meta?.cursor || '', res.meta?.has_more || false)
    } catch {
      // ignore
    }
  }, [channelId, setMessages])

  useEffect(() => {
    if (channelId && !messages[channelId]) {
      loadMessages()
    }
  }, [channelId, messages, loadMessages])

  const handleLoadMore = async () => {
    if (!channelId || !currentHasMore || !currentCursor) return
    try {
      const res = await messageApi.list(channelId, currentCursor)
      useMessageStore.getState().appendMessages(channelId, res.data, res.meta?.cursor || '', res.meta?.has_more || false)
    } catch {
      // ignore
    }
  }

  const handleSend = async (content: string) => {
    if (!channelId || !content.trim()) return
    try {
      await messageApi.send(channelId, content)
      // Message will arrive via WebSocket or we add it directly
    } catch {
      // ignore
    }
  }

  return (
    <>
      {/* Channel header */}
      <header className="h-14 px-5 flex items-center border-b border-surface-700/50 bg-surface-950 shrink-0">
        <span className="text-surface-200/60 mr-2">#</span>
        <h2 className="font-semibold text-white">{activeChannel?.name || 'Channel'}</h2>
        {activeChannel?.topic && (
          <span className="ml-3 text-sm text-surface-200/60 truncate">{activeChannel.topic}</span>
        )}
      </header>

      {/* Messages */}
      <MessageList
        messages={currentMessages}
        hasMore={currentHasMore}
        onLoadMore={handleLoadMore}
      />

      {/* Input */}
      <MessageInput onSend={handleSend} channelName={activeChannel?.name || 'channel'} />
    </>
  )
}
