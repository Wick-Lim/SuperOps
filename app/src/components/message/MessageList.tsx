import React, { useRef } from 'react'
import { FlatList, View, Text, ActivityIndicator, NativeSyntheticEvent, NativeScrollEvent } from 'react-native'
import type { Message } from '../../lib/types'
import { theme } from '../../lib/theme'
import MessageItem from './MessageItem'

interface Props {
  messages: Message[]
  onLongPress?: (message: Message) => void
  onAddReaction?: (message: Message) => void
  onLoadMore?: () => void
  loadingMore?: boolean
}

export default function MessageList({ messages, onLongPress, onAddReaction, onLoadMore, loadingMore }: Props) {
  const listRef = useRef<FlatList>(null)
  const autoScroll = useRef(true)

  if (messages.length === 0) {
    return (
      <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
        <Text style={{ color: theme.textDim, fontSize: 15 }}>No messages yet. Start the conversation!</Text>
      </View>
    )
  }

  const handleScroll = (e: NativeSyntheticEvent<NativeScrollEvent>) => {
    const y = e.nativeEvent.contentOffset.y
    const { layoutMeasurement, contentSize } = e.nativeEvent
    // Near the bottom -> keep auto-scrolling on new content.
    autoScroll.current = y + layoutMeasurement.height >= contentSize.height - 80
    // Near the top -> fetch the previous page of older messages.
    if (y <= 40 && onLoadMore) onLoadMore()
  }

  return (
    <FlatList
      ref={listRef}
      data={messages}
      keyExtractor={(m) => m.id}
      renderItem={({ item, index }) => {
        const prev = index > 0 ? messages[index - 1] : null
        const showHeader =
          !prev ||
          prev.user_id !== item.user_id ||
          new Date(item.created_at).getTime() - new Date(prev.created_at).getTime() > 300000
        return (
          <MessageItem
            message={item}
            showHeader={showHeader}
            onLongPress={onLongPress}
            onAddReaction={onAddReaction}
          />
        )
      }}
      contentContainerStyle={{ paddingHorizontal: 16, paddingVertical: 8 }}
      onScroll={handleScroll}
      scrollEventThrottle={64}
      ListHeaderComponent={
        loadingMore ? (
          <View style={{ paddingVertical: 12, alignItems: 'center' }}>
            <ActivityIndicator size="small" color={theme.textMuted} />
          </View>
        ) : null
      }
      onContentSizeChange={() => {
        if (autoScroll.current) listRef.current?.scrollToEnd({ animated: false })
      }}
      onLayout={() => listRef.current?.scrollToEnd({ animated: false })}
    />
  )
}
