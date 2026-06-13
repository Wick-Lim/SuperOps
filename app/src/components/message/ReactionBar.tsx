import React from 'react'
import { View, Text, Pressable, Alert } from 'react-native'
import type { Message, Reaction } from '../../lib/types'
import { theme } from '../../lib/theme'
import { messageApi } from '../../api/messages'
import { useAuthStore } from '../../stores/authStore'
import { useMessageStore } from '../../stores/messageStore'

interface Props {
  message: Message
  onAddReaction?: (message: Message) => void
}

interface Group {
  emoji: string
  count: number
  mine: boolean
}

function groupReactions(reactions: Reaction[], userId: string | undefined): Group[] {
  const map = new Map<string, Group>()
  for (const r of reactions) {
    const g = map.get(r.emoji) ?? { emoji: r.emoji, count: 0, mine: false }
    g.count += 1
    if (userId && r.user_id === userId) g.mine = true
    map.set(r.emoji, g)
  }
  return Array.from(map.values())
}

export default function ReactionBar({ message, onAddReaction }: Props) {
  const userId = useAuthStore((s) => s.user?.id)
  const reactions = message.reactions ?? []
  const groups = groupReactions(reactions, userId)

  const toggle = async (emoji: string, mine: boolean) => {
    if (!userId) return
    const apply = useMessageStore.getState().applyReaction
    // Optimistic update.
    const optimistic: Reaction = {
      id: '',
      message_id: message.id,
      user_id: userId,
      emoji,
      created_at: new Date().toISOString(),
    }
    apply(message.channel_id, optimistic, !mine)
    try {
      if (mine) await messageApi.unreact(message.channel_id, message.id, emoji)
      else await messageApi.react(message.channel_id, message.id, emoji)
    } catch (err) {
      // Roll back on failure.
      apply(message.channel_id, optimistic, mine)
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not update reaction')
    }
  }

  if (groups.length === 0 && !onAddReaction) return null

  return (
    <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6, marginTop: 6 }}>
      {groups.map((g) => (
        <Pressable
          key={g.emoji}
          onPress={() => toggle(g.emoji, g.mine)}
          style={{
            flexDirection: 'row',
            alignItems: 'center',
            gap: 4,
            paddingHorizontal: 8,
            paddingVertical: 3,
            borderRadius: 12,
            borderWidth: 1,
            backgroundColor: g.mine ? '#312e81' : theme.surface,
            borderColor: g.mine ? theme.primary : theme.border,
          }}
        >
          <Text style={{ fontSize: 13 }}>{g.emoji}</Text>
          <Text style={{ color: g.mine ? theme.accent : theme.textMuted, fontSize: 12, fontWeight: '600' }}>
            {g.count}
          </Text>
        </Pressable>
      ))}

      {onAddReaction && (
        <Pressable
          onPress={() => onAddReaction(message)}
          style={{
            paddingHorizontal: 9,
            paddingVertical: 3,
            borderRadius: 12,
            borderWidth: 1,
            backgroundColor: theme.surface,
            borderColor: theme.border,
          }}
        >
          <Text style={{ color: theme.textMuted, fontSize: 13 }}>＋</Text>
        </Pressable>
      )}
    </View>
  )
}
