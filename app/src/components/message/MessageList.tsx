import React, { useRef } from 'react'
import { FlatList, View, Text } from 'react-native'
import type { Message } from '../../lib/types'
import MessageItem from './MessageItem'

interface Props {
  messages: Message[]
}

export default function MessageList({ messages }: Props) {
  const listRef = useRef<FlatList>(null)

  if (messages.length === 0) {
    return (
      <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
        <Text style={{ color: '#475569', fontSize: 15 }}>No messages yet. Start the conversation!</Text>
      </View>
    )
  }

  return (
    <FlatList
      ref={listRef}
      data={messages}
      keyExtractor={(m) => m.id}
      renderItem={({ item, index }) => {
        const prev = index > 0 ? messages[index - 1] : null
        const showHeader = !prev || prev.user_id !== item.user_id ||
          new Date(item.created_at).getTime() - new Date(prev.created_at).getTime() > 300000
        return <MessageItem message={item} showHeader={showHeader} />
      }}
      contentContainerStyle={{ paddingHorizontal: 16, paddingVertical: 8 }}
      onContentSizeChange={() => listRef.current?.scrollToEnd({ animated: false })}
      onLayout={() => listRef.current?.scrollToEnd({ animated: false })}
    />
  )
}
