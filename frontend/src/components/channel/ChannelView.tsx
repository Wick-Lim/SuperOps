import { useEffect, useCallback, useRef } from 'react'
import { messageApi } from '@/api/messages'
import { useChannelStore } from '@/stores/channelStore'
import { useMessageStore } from '@/stores/messageStore'
import type { Message } from '@/lib/types'
import MessageList from '@/components/message/MessageList'
import MessageInput from '@/components/message/MessageInput'
import TypingIndicator from '@/components/presence/TypingIndicator'
import { shallow } from 'zustand/shallow'

interface ChannelViewProps {
  channelId: string
}

const EMPTY_MESSAGES: Message[] = []

export default function ChannelView({ channelId }: ChannelViewProps) {
  const activeChannel = useChannelStore((s) => s.channels.find((c) => c.id === channelId))
  const currentMessages = useMessageStore((s) => s.messages[channelId] ?? EMPTY_MESSAGES)
  const currentCursor = useMessageStore((s) => s.cursors[channelId])
  const currentHasMore = useMessageStore((s) => s.hasMore[channelId] ?? true)
  const setMessages = useMessageStore((s) => s.setMessages)

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
    if (channelId) {
      loadMessages()
    }
  }, [channelId, loadMessages])

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
      const res = await messageApi.send(channelId, content)
      useMessageStore.getState().addMessage(channelId, res.data)
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

      {/* Typing indicator */}
      {channelId && <TypingIndicator channelId={channelId} />}

      {/* Input */}
      <MessageInput onSend={handleSend} channelName={activeChannel?.name || 'channel'} channelId={channelId} />
    </>
  )
}
