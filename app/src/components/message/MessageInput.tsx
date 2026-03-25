import React, { useState } from 'react'
import { View, TextInput, Pressable, Text } from 'react-native'

interface Props {
  onSend: (content: string) => void
  channelName: string
}

export default function MessageInput({ onSend, channelName }: Props) {
  const [content, setContent] = useState('')

  const handleSend = () => {
    if (!content.trim()) return
    onSend(content.trim())
    setContent('')
  }

  return (
    <View style={{ flexDirection: 'row', alignItems: 'flex-end', paddingHorizontal: 12, paddingVertical: 8, borderTopWidth: 1, borderTopColor: '#1e293b', backgroundColor: '#020617', gap: 8 }}>
      <TextInput
        value={content}
        onChangeText={setContent}
        placeholder={`Message #${channelName}`}
        placeholderTextColor="#475569"
        multiline
        onSubmitEditing={handleSend}
        blurOnSubmit={false}
        style={{
          flex: 1,
          backgroundColor: '#0f172a',
          borderWidth: 1,
          borderColor: '#334155',
          borderRadius: 12,
          paddingHorizontal: 14,
          paddingVertical: 10,
          color: '#fff',
          fontSize: 15,
          maxHeight: 120,
        }}
      />
      <Pressable
        onPress={handleSend}
        disabled={!content.trim()}
        style={{
          backgroundColor: content.trim() ? '#4f46e5' : '#1e293b',
          borderRadius: 12,
          paddingHorizontal: 16,
          paddingVertical: 10,
        }}
      >
        <Text style={{ color: '#fff', fontWeight: '600', fontSize: 14 }}>Send</Text>
      </Pressable>
    </View>
  )
}
