import React from 'react'
import { View, Text, Pressable } from 'react-native'
import { theme, avatarColor } from '../../lib/theme'
import { displayName } from '../../stores/userStore'
import type { Message, PublicUser } from '../../lib/types'
import { MIN_TOUCH } from './ui'

/**
 * Compact read-only message row for the pin / bookmark / scheduled lists.
 *
 * Deliberately not `components/message/MessageItem`: those rows carry reaction
 * bars, attachment rendering and per-row store subscriptions that make no sense
 * in a flat list of references.
 */
export default function MessageRow({
  message,
  users,
  meta,
  onPress,
  actionLabel,
  onAction,
  actionDestructive,
  actionBusy,
}: {
  message: Message
  users: Record<string, PublicUser>
  /** Secondary line: channel name, schedule time, bookmark date… */
  meta?: string
  onPress?: () => void
  actionLabel?: string
  onAction?: () => void
  actionDestructive?: boolean
  actionBusy?: boolean
}) {
  const author = displayName(users, message.user_id)
  const body = (
    <>
      <View
        style={{
          width: 36,
          height: 36,
          borderRadius: 10,
          backgroundColor: avatarColor(message.user_id),
          alignItems: 'center',
          justifyContent: 'center',
          marginRight: 10,
        }}
      >
        <Text style={{ color: '#fff', fontSize: 12, fontWeight: '700' }}>
          {author.slice(0, 2).toUpperCase()}
        </Text>
      </View>
      <View style={{ flex: 1 }}>
        <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }}>{author}</Text>
          {meta ? (
            <Text style={{ color: theme.textMuted, fontSize: 12, flex: 1 }} numberOfLines={1}>
              {meta}
            </Text>
          ) : null}
        </View>
        <Text style={{ color: theme.body, fontSize: 15, lineHeight: 21, marginTop: 2 }} numberOfLines={4}>
          {message.content}
        </Text>
        {(message.files?.length ?? 0) > 0 ? (
          <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 4 }}>
            📎 {message.files?.length} attachment{(message.files?.length ?? 0) === 1 ? '' : 's'}
          </Text>
        ) : null}
      </View>
    </>
  )

  return (
    <View
      style={{
        flexDirection: 'row',
        alignItems: 'flex-start',
        paddingHorizontal: 16,
        paddingVertical: 12,
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
      }}
    >
      {onPress ? (
        <Pressable
          onPress={onPress}
          accessibilityRole="button"
          accessibilityLabel={`Message from ${author}: ${message.content}`}
          accessibilityHint="Opens the conversation"
          style={{ flexDirection: 'row', flex: 1, minHeight: MIN_TOUCH }}
        >
          {body}
        </Pressable>
      ) : (
        <View
          accessible
          accessibilityLabel={`Message from ${author}: ${message.content}`}
          style={{ flexDirection: 'row', flex: 1 }}
        >
          {body}
        </View>
      )}

      {actionLabel && onAction ? (
        <Pressable
          onPress={onAction}
          disabled={actionBusy}
          accessibilityRole="button"
          accessibilityLabel={`${actionLabel}, message from ${author}`}
          accessibilityState={{ disabled: !!actionBusy, busy: !!actionBusy }}
          hitSlop={8}
          style={{
            marginLeft: 8,
            paddingHorizontal: 10,
            minHeight: MIN_TOUCH,
            justifyContent: 'center',
            opacity: actionBusy ? 0.5 : 1,
          }}
        >
          <Text
            style={{
              color: actionDestructive ? theme.danger : theme.accent,
              fontSize: 13,
              fontWeight: '600',
            }}
          >
            {actionLabel}
          </Text>
        </Pressable>
      ) : null}
    </View>
  )
}
