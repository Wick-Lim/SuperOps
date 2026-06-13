import React from 'react'
import { Modal, View, Text, Pressable, ScrollView } from 'react-native'
import { theme } from '../../lib/theme'

interface Props {
  visible: boolean
  onClose: () => void
  onSelect: (emoji: string) => void
}

const EMOJIS = [
  '👍', '👎', '❤️', '😂', '🎉', '🙏', '🔥', '👀',
  '😍', '😎', '🤔', '😢', '😮', '😡', '🥳', '😴',
  '✅', '❌', '⭐', '💯', '🚀', '👏', '🙌', '💪',
  '🤝', '👋', '🤩', '😅', '😬', '🤯', '💀', '🤖',
  '🍕', '☕', '🎯', '💡', '⚡', '🌟', '✨', '❓',
]

export default function EmojiPicker({ visible, onClose, onSelect }: Props) {
  return (
    <Modal visible={visible} transparent animationType="fade" onRequestClose={onClose}>
      <Pressable
        onPress={onClose}
        style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.6)', justifyContent: 'flex-end' }}
      >
        <Pressable
          onPress={(e) => e.stopPropagation()}
          style={{
            backgroundColor: theme.surface,
            borderTopLeftRadius: 20,
            borderTopRightRadius: 20,
            paddingHorizontal: 16,
            paddingTop: 12,
            paddingBottom: 28,
            borderTopWidth: 1,
            borderColor: theme.border,
          }}
        >
          <View style={{ alignItems: 'center', marginBottom: 12 }}>
            <View style={{ width: 40, height: 4, borderRadius: 2, backgroundColor: theme.borderStrong }} />
          </View>
          <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '700', letterSpacing: 1, marginBottom: 10 }}>
            PICK A REACTION
          </Text>
          <ScrollView style={{ maxHeight: 260 }}>
            <View style={{ flexDirection: 'row', flexWrap: 'wrap' }}>
              {EMOJIS.map((emoji) => (
                <Pressable
                  key={emoji}
                  onPress={() => {
                    onSelect(emoji)
                    onClose()
                  }}
                  style={{
                    width: '12.5%',
                    aspectRatio: 1,
                    alignItems: 'center',
                    justifyContent: 'center',
                  }}
                >
                  <Text style={{ fontSize: 26 }}>{emoji}</Text>
                </Pressable>
              ))}
            </View>
          </ScrollView>
        </Pressable>
      </Pressable>
    </Modal>
  )
}
