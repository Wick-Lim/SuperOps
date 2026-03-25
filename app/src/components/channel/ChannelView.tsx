import React, { useEffect, useCallback, useState } from 'react'
import { View, Text, Pressable, KeyboardAvoidingView, Platform } from 'react-native'
import { messageApi } from '../../api/messages'
import { useMessageStore } from '../../stores/messageStore'
import type { Channel, Message } from '../../lib/types'
import MessageList from '../message/MessageList'
import MessageInput from '../message/MessageInput'

interface Props {
  channel: Channel
  onBack: () => void
}

const EMPTY: Message[] = []

export default function ChannelView({ channel, onBack }: Props) {
  const currentMessages = useMessageStore((s) => s.messages[channel.id] ?? EMPTY)
  const setMessages = useMessageStore((s) => s.setMessages)

  const loadMessages = useCallback(async () => {
    try {
      const res = await messageApi.list(channel.id)
      setMessages(channel.id, res.data, res.meta?.cursor || '', res.meta?.has_more || false)
    } catch {}
  }, [channel.id, setMessages])

  useEffect(() => { loadMessages() }, [loadMessages])

  const handleSend = async (content: string) => {
    if (!content.trim()) return
    try {
      const res = await messageApi.send(channel.id, content)
      useMessageStore.getState().addMessage(channel.id, res.data)
    } catch {}
  }

  return (
    <KeyboardAvoidingView style={{ flex: 1 }} behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
      {/* Header */}
      <View style={{ height: 56, paddingHorizontal: 16, flexDirection: 'row', alignItems: 'center', borderBottomWidth: 1, borderBottomColor: '#1e293b', backgroundColor: '#020617' }}>
        <Pressable onPress={onBack} style={{ marginRight: 12, padding: 4 }}>
          <Text style={{ color: '#94a3b8', fontSize: 18 }}>←</Text>
        </Pressable>
        <Text style={{ color: '#64748b', marginRight: 4 }}>#</Text>
        <Text style={{ color: '#fff', fontWeight: '600', fontSize: 16 }}>{channel.name || 'Channel'}</Text>
      </View>

      {/* Messages */}
      <MessageList messages={currentMessages} />

      {/* Input */}
      <MessageInput onSend={handleSend} channelName={channel.name || 'channel'} />
    </KeyboardAvoidingView>
  )
}
