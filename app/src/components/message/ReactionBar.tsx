import React, { useMemo } from 'react'
import { Alert, Image, Pressable, Text, View } from 'react-native'
import type { Message, Reaction } from '../../lib/types'
import { theme } from '../../lib/theme'
import { messageApi } from '../../api/messages'
import { errorMessage } from '../../api/client'
import { useAuthStore } from '../../stores/authStore'
import { useMessageStore } from '../../stores/messageStore'
import type { CustomEmoji } from '../../api/emoji'
import { customEmojiUrl, findCustomEmoji } from './customEmoji'
import { anchorFrom, type Anchor } from './anchor'
import { touchSlop } from '../a11y'

interface Props {
  message: Message
  /**
   * `anchor` is where the press landed, so a pointer-driven picker can open
   * beside the ＋ instead of sliding up from the bottom of the window.
   */
  onAddReaction?: (message: Message, anchor?: Anchor) => void
  /**
   * Overrides the default store-backed toggle. Thread replies are not in
   * `messageStore` (the channel list excludes them server-side), so
   * `applyReaction` would be a silent no-op for them.
   */
  onToggle?: (message: Message, emoji: string, mine: boolean) => void
  /** Workspace custom emoji, so a `:name:` reaction renders as its image. */
  customEmoji?: CustomEmoji[]
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

const NO_EMOJI: CustomEmoji[] = []

function ReactionBarBase({ message, onAddReaction, onToggle, customEmoji = NO_EMOJI }: Props) {
  const userId = useAuthStore((s) => s.user?.id)
  const reactions = message.reactions ?? []
  const groups = useMemo(() => groupReactions(reactions, userId), [reactions, userId])

  const toggle = async (emoji: string, mine: boolean) => {
    if (!userId) return
    if (onToggle) {
      onToggle(message, emoji, mine)
      return
    }
    const apply = useMessageStore.getState().applyReaction
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
      apply(message.channel_id, optimistic, mine)
      Alert.alert('Error', errorMessage(err, 'Could not update reaction'))
    }
  }

  if (groups.length === 0 && !onAddReaction) return null

  return (
    <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6, marginTop: 6 }}>
      {groups.map((g) => {
        const custom = findCustomEmoji(customEmoji, g.emoji)
        return (
          <Pressable
            key={g.emoji}
            onPress={() => toggle(g.emoji, g.mine)}
            hitSlop={touchSlop(28)}
            accessibilityRole="button"
            accessibilityState={{ selected: g.mine }}
            accessibilityLabel={`${g.emoji} ${g.count}`}
            accessibilityHint={g.mine ? 'Removes your reaction' : 'Adds your reaction'}
            style={{
              flexDirection: 'row',
              alignItems: 'center',
              gap: 4,
              paddingHorizontal: 8,
              paddingVertical: 4,
              borderRadius: 12,
              borderWidth: 1,
              backgroundColor: g.mine ? '#312e81' : theme.surface,
              borderColor: g.mine ? theme.primary : theme.border,
            }}
          >
            {custom ? (
              <Image
                source={{ uri: customEmojiUrl(custom) }}
                style={{ width: 16, height: 16 }}
                resizeMode="contain"
              />
            ) : (
              <Text style={{ fontSize: 13 }}>{g.emoji}</Text>
            )}
            <Text style={{ color: g.mine ? theme.accent : theme.textMuted, fontSize: 12, fontWeight: '600' }}>
              {g.count}
            </Text>
          </Pressable>
        )
      })}

      {onAddReaction && (
        <Pressable
          onPress={(e) => onAddReaction(message, anchorFrom(e))}
          hitSlop={touchSlop(28)}
          accessibilityRole="button"
          accessibilityLabel="Add reaction"
          style={{
            minWidth: 32,
            paddingHorizontal: 9,
            paddingVertical: 4,
            borderRadius: 12,
            borderWidth: 1,
            alignItems: 'center',
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

export default React.memo(ReactionBarBase)
