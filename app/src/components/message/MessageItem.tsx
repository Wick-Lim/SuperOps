import React from 'react'
import { View, Text } from 'react-native'
import type { Message } from '../../lib/types'

interface Props {
  message: Message
  showHeader: boolean
}

function formatTime(dateStr: string) {
  const d = new Date(dateStr)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

export default function MessageItem({ message, showHeader }: Props) {
  const initials = message.user_id.slice(0, 2).toUpperCase()
  const hue = message.user_id.split('').reduce((a, c) => a + c.charCodeAt(0), 0) % 360
  const color = `hsl(${hue}, 60%, 50%)`

  return (
    <View style={{ flexDirection: 'row', marginTop: showHeader ? 16 : 2, gap: 10 }}>
      {showHeader ? (
        <View style={{ width: 36, height: 36, borderRadius: 10, backgroundColor: color, alignItems: 'center', justifyContent: 'center' }}>
          <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>{initials}</Text>
        </View>
      ) : (
        <View style={{ width: 36 }} />
      )}

      <View style={{ flex: 1 }}>
        {showHeader && (
          <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8, marginBottom: 2 }}>
            <Text style={{ color: '#fff', fontSize: 14, fontWeight: '600' }}>{message.user_id.slice(0, 8)}</Text>
            <Text style={{ color: '#475569', fontSize: 11 }}>{formatTime(message.created_at)}</Text>
            {message.is_edited && <Text style={{ color: '#475569', fontSize: 11 }}>(edited)</Text>}
          </View>
        )}
        <Text style={{ color: '#e2e8f0', fontSize: 15, lineHeight: 22 }}>{message.content}</Text>
        {message.reply_count > 0 && (
          <Text style={{ color: '#818cf8', fontSize: 12, marginTop: 4 }}>
            {message.reply_count} {message.reply_count === 1 ? 'reply' : 'replies'}
          </Text>
        )}
      </View>
    </View>
  )
}
