import React, { useEffect, useCallback, useRef, useState } from 'react'
import {
  View,
  Text,
  Pressable,
  KeyboardAvoidingView,
  Platform,
  Alert,
  Modal,
  TextInput,
  ScrollView,
} from 'react-native'
import { messageApi } from '../../api/messages'
import { useMessageStore } from '../../stores/messageStore'
import { useAuthStore } from '../../stores/authStore'
import { useUiStore } from '../../stores/uiStore'
import { useChannelStore } from '../../stores/channelStore'
import { useUserStore, displayName } from '../../stores/userStore'
import { theme } from '../../lib/theme'
import type { Channel, Message } from '../../lib/types'
import MessageList from '../message/MessageList'
import MessageInput from '../message/MessageInput'
import EmojiPicker from '../message/EmojiPicker'

interface Props {
  channel: Channel
  onBack: () => void
  onOpenMembers?: () => void
}

const EMPTY: Message[] = []
const NO_TYPING: string[] = []

export default function ChannelView({ channel, onBack, onOpenMembers }: Props) {
  const currentMessages = useMessageStore((s) => s.messages[channel.id] ?? EMPTY)
  const setMessages = useMessageStore((s) => s.setMessages)
  const prependMessages = useMessageStore((s) => s.prependMessages)
  const currentUserId = useAuthStore((s) => s.user?.id)
  const typingIds = useUiStore((s) => s.typing[channel.id] ?? NO_TYPING)
  const channels = useChannelStore((s) => s.channels)
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [loadingMore, setLoadingMore] = useState(false)
  const [emojiTarget, setEmojiTarget] = useState<Message | null>(null)
  const [editTarget, setEditTarget] = useState<Message | null>(null)
  const [editText, setEditText] = useState('')
  const [forwardTarget, setForwardTarget] = useState<Message | null>(null)
  const loadingMoreRef = useRef(false)

  const loadMessages = useCallback(async () => {
    try {
      const res = await messageApi.list(channel.id)
      setMessages(channel.id, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not load messages')
    }
  }, [channel.id, setMessages])

  useEffect(() => {
    loadMessages()
  }, [loadMessages])

  // Resolve names for users currently typing.
  useEffect(() => {
    if (typingIds.length) ensureUsers(typingIds)
  }, [typingIds, ensureUsers])

  const handleLoadMore = useCallback(async () => {
    if (loadingMoreRef.current) return
    const { cursors, hasMore } = useMessageStore.getState()
    if (!hasMore[channel.id]) return
    const cursor = cursors[channel.id]
    if (!cursor) return
    loadingMoreRef.current = true
    setLoadingMore(true)
    try {
      const res = await messageApi.list(channel.id, cursor)
      prependMessages(channel.id, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
    } catch {
      // Silent — pagination failures shouldn't interrupt reading.
    } finally {
      loadingMoreRef.current = false
      setLoadingMore(false)
    }
  }, [channel.id, prependMessages])

  const handleSend = async (content: string, fileIds?: string[]) => {
    if (!content.trim() && (!fileIds || fileIds.length === 0)) return
    try {
      const res = await messageApi.send(channel.id, content, fileIds ? { fileIds } : undefined)
      useMessageStore.getState().addMessage(channel.id, res.data)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not send message')
    }
  }

  const reactWithEmoji = async (message: Message, emoji: string) => {
    if (!currentUserId) return
    const apply = useMessageStore.getState().applyReaction
    apply(channel.id, { id: '', message_id: message.id, user_id: currentUserId, emoji, created_at: new Date().toISOString() }, true)
    try {
      await messageApi.react(channel.id, message.id, emoji)
    } catch (err) {
      apply(channel.id, { id: '', message_id: message.id, user_id: currentUserId, emoji, created_at: '' }, false)
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not add reaction')
    }
  }

  const doPin = async (m: Message) => {
    try {
      if (m.is_pinned) await messageApi.unpin(channel.id, m.id)
      else await messageApi.pin(channel.id, m.id)
      useMessageStore.getState().updateMessage(channel.id, { ...m, is_pinned: !m.is_pinned })
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not pin message')
    }
  }

  const doBookmark = async (m: Message) => {
    try {
      await messageApi.bookmark(m.id)
      Alert.alert('Saved', 'Message bookmarked')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not bookmark message')
    }
  }

  const doDelete = (m: Message) => {
    Alert.alert('Delete message', 'This cannot be undone.', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Delete',
        style: 'destructive',
        onPress: async () => {
          try {
            await messageApi.del(channel.id, m.id)
            useMessageStore.getState().removeMessage(channel.id, m.id)
          } catch (err) {
            Alert.alert('Error', err instanceof Error ? err.message : 'Could not delete message')
          }
        },
      },
    ])
  }

  const submitEdit = async () => {
    if (!editTarget) return
    const next = editText.trim()
    if (!next) return
    try {
      const res = await messageApi.edit(channel.id, editTarget.id, next)
      useMessageStore.getState().updateMessage(channel.id, res.data)
      setEditTarget(null)
      setEditText('')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not edit message')
    }
  }

  const doForward = async (m: Message, targetChannelId: string) => {
    try {
      await messageApi.forward(channel.id, m.id, targetChannelId)
      setForwardTarget(null)
      Alert.alert('Forwarded', 'Message forwarded')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Could not forward message')
    }
  }

  const openActions = (m: Message) => {
    const isOwn = m.user_id === currentUserId
    const options: Array<{ text: string; onPress?: () => void; style?: 'cancel' | 'destructive' }> = [
      { text: 'React', onPress: () => setEmojiTarget(m) },
      { text: 'Pin', onPress: () => doPin(m) },
      { text: 'Bookmark', onPress: () => doBookmark(m) },
      { text: 'Forward', onPress: () => setForwardTarget(m) },
    ]
    if (isOwn) {
      options.push({
        text: 'Edit',
        onPress: () => {
          setEditTarget(m)
          setEditText(m.content)
        },
      })
      options.push({ text: 'Delete', style: 'destructive', onPress: () => doDelete(m) })
    }
    options.push({ text: 'Cancel', style: 'cancel' })
    Alert.alert('Message', undefined, options)
  }

  const typingLabel = (() => {
    const names = typingIds.map((id) => displayName(users, id))
    if (names.length === 0) return null
    if (names.length === 1) return `${names[0]} is typing…`
    if (names.length === 2) return `${names[0]} and ${names[1]} are typing…`
    return 'Several people are typing…'
  })()

  return (
    <KeyboardAvoidingView style={{ flex: 1 }} behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
      {/* Header */}
      <View
        style={{
          height: 56,
          paddingHorizontal: 16,
          flexDirection: 'row',
          alignItems: 'center',
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
          backgroundColor: theme.bg,
        }}
      >
        <Pressable onPress={onBack} style={{ marginRight: 12, padding: 4 }}>
          <Text style={{ color: theme.textMuted, fontSize: 18 }}>←</Text>
        </Pressable>
        <Text style={{ color: theme.textFaint, marginRight: 4 }}>
          {channel.type === 'dm' || channel.type === 'group_dm' ? '💬' : '#'}
        </Text>
        <Text style={{ color: theme.text, fontWeight: '600', fontSize: 16, flex: 1 }} numberOfLines={1}>
          {channel.name || (channel.type === 'dm' ? 'Direct message' : 'Channel')}
        </Text>
        {onOpenMembers && (
          <Pressable onPress={onOpenMembers} style={{ padding: 6 }}>
            <Text style={{ color: theme.textMuted, fontSize: 16 }}>👥</Text>
          </Pressable>
        )}
      </View>

      {/* Messages */}
      <MessageList
        messages={currentMessages}
        onLongPress={openActions}
        onAddReaction={(m) => setEmojiTarget(m)}
        onLoadMore={handleLoadMore}
        loadingMore={loadingMore}
      />

      {/* Typing indicator */}
      {typingLabel && (
        <View style={{ paddingHorizontal: 16, paddingBottom: 4 }}>
          <Text style={{ color: theme.textFaint, fontSize: 12, fontStyle: 'italic' }}>{typingLabel}</Text>
        </View>
      )}

      {/* Input */}
      <MessageInput onSend={handleSend} channelName={channel.name || 'channel'} channelId={channel.id} />

      {/* Emoji picker (React action / + reaction) */}
      <EmojiPicker
        visible={!!emojiTarget}
        onClose={() => setEmojiTarget(null)}
        onSelect={(emoji) => {
          if (emojiTarget) reactWithEmoji(emojiTarget, emoji)
        }}
      />

      {/* Edit modal */}
      <Modal visible={!!editTarget} transparent animationType="fade" onRequestClose={() => setEditTarget(null)}>
        <View style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.6)', justifyContent: 'center', paddingHorizontal: 24 }}>
          <View style={{ backgroundColor: theme.surface, borderRadius: 16, padding: 16, borderWidth: 1, borderColor: theme.border }}>
            <Text style={{ color: theme.text, fontSize: 15, fontWeight: '600', marginBottom: 12 }}>Edit message</Text>
            <TextInput
              value={editText}
              onChangeText={setEditText}
              multiline
              autoFocus
              placeholderTextColor={theme.textDim}
              style={{
                backgroundColor: theme.bg,
                borderWidth: 1,
                borderColor: theme.borderStrong,
                borderRadius: 12,
                padding: 12,
                color: theme.text,
                fontSize: 15,
                minHeight: 80,
                maxHeight: 160,
              }}
            />
            <View style={{ flexDirection: 'row', justifyContent: 'flex-end', gap: 8, marginTop: 14 }}>
              <Pressable
                onPress={() => {
                  setEditTarget(null)
                  setEditText('')
                }}
                style={{ paddingHorizontal: 16, paddingVertical: 10, borderRadius: 10, backgroundColor: theme.surfaceAlt }}
              >
                <Text style={{ color: theme.textMuted, fontWeight: '600' }}>Cancel</Text>
              </Pressable>
              <Pressable
                onPress={submitEdit}
                style={{ paddingHorizontal: 16, paddingVertical: 10, borderRadius: 10, backgroundColor: theme.primary }}
              >
                <Text style={{ color: '#fff', fontWeight: '600' }}>Save</Text>
              </Pressable>
            </View>
          </View>
        </View>
      </Modal>

      {/* Forward modal — pick a target channel */}
      <Modal visible={!!forwardTarget} transparent animationType="fade" onRequestClose={() => setForwardTarget(null)}>
        <Pressable
          onPress={() => setForwardTarget(null)}
          style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.6)', justifyContent: 'flex-end' }}
        >
          <Pressable
            onPress={(e) => e.stopPropagation()}
            style={{
              backgroundColor: theme.surface,
              borderTopLeftRadius: 20,
              borderTopRightRadius: 20,
              paddingHorizontal: 16,
              paddingTop: 16,
              paddingBottom: 28,
              borderTopWidth: 1,
              borderColor: theme.border,
              maxHeight: '70%',
            }}
          >
            <Text style={{ color: theme.text, fontSize: 15, fontWeight: '600', marginBottom: 12 }}>Forward to…</Text>
            <ScrollView>
              {channels
                .filter((c) => c.id !== channel.id)
                .map((c) => (
                  <Pressable
                    key={c.id}
                    onPress={() => forwardTarget && doForward(forwardTarget, c.id)}
                    style={{ flexDirection: 'row', alignItems: 'center', paddingVertical: 12 }}
                  >
                    <Text style={{ color: theme.textFaint, marginRight: 6 }}>#</Text>
                    <Text style={{ color: theme.body, fontSize: 15 }}>{c.name || 'channel'}</Text>
                  </Pressable>
                ))}
            </ScrollView>
          </Pressable>
        </Pressable>
      </Modal>
    </KeyboardAvoidingView>
  )
}
