import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import HuddleBar from './HuddleBar'
import {
  Alert,
  KeyboardAvoidingView,
  Linking,
  Modal,
  Platform,
  Pressable,
  ScrollView,
  Text,
  TextInput,
  View,
} from 'react-native'
import { messageApi } from '../../api/messages'
import { channelApi } from '../../api/channels'
import { errorMessage, fileURL } from '../../api/client'
import type { UploadedFile } from '../../api/files'
import { useMessageStore } from '../../stores/messageStore'
import { useAuthStore } from '../../stores/authStore'
import { useUiStore } from '../../stores/uiStore'
import { useChannelStore } from '../../stores/channelStore'
import { useUserStore, displayName } from '../../stores/userStore'
import { theme } from '../../lib/theme'
import { space, useResponsive } from '../../lib/responsive'
import type { Channel, FileRef, Message } from '../../lib/types'
import MessageList from '../message/MessageList'
import MessageInput from '../message/MessageInput'
import EmojiPicker from '../message/EmojiPicker'
import AttachmentViewer from '../message/AttachmentViewer'
import ActionSheet, { type SheetAction } from '../ActionSheet'
import type { SendState } from '../message/MessageItem'
import type { Anchor } from '../message/anchor'
import { newTempId, optimisticMessage, type OutboxEntry } from '../message/outbox'
import { useCustomEmoji } from '../message/customEmoji'
import { announce, useModalFocus } from '../a11y'

interface Props {
  channel: Channel
  /** Only reachable below `medium`; a fixed sidebar is its own way back. */
  onBack: () => void
  onOpenMembers?: () => void
  /**
   * Whether to offer a way back to the channel list. Defaults to "only when the
   * list is not already on screen", so a shell that knows better can override it
   * but does not have to.
   */
  showBack?: boolean
}

const EMPTY: Message[] = []
const NO_TYPING: string[] = []

/** DM participant ids by channel, so reopening a DM costs no extra request. */
const dmMembers = new Map<string, string[]>()

export default function ChannelView({ channel, onBack, onOpenMembers, showBack }: Props) {
  const currentMessages = useMessageStore((s) => s.messages[channel.id] ?? EMPTY)
  const setMessages = useMessageStore((s) => s.setMessages)
  const prependMessages = useMessageStore((s) => s.prependMessages)
  const currentUserId = useAuthStore((s) => s.user?.id)
  const typingIds = useUiStore((s) => s.typing[channel.id] ?? NO_TYPING)
  const channels = useChannelStore((s) => s.channels)
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)
  const customEmoji = useCustomEmoji()
  const { minTouch, gutter, twoPane } = useResponsive()
  const backVisible = showBack ?? !twoPane
  /** A phone header has room for the name or the topic, not both. */
  const showTopic = twoPane && !!channel.topic

  const [loadingMore, setLoadingMore] = useState(false)
  const [loadMoreError, setLoadMoreError] = useState<string | null>(null)
  const [loadError, setLoadError] = useState<string | null>(null)
  const [outbox, setOutbox] = useState<Record<string, OutboxEntry>>({})
  const [scrollKey, setScrollKey] = useState(0)

  const [actionTarget, setActionTarget] = useState<Message | null>(null)
  const [emojiTarget, setEmojiTarget] = useState<Message | null>(null)
  const [emojiAnchor, setEmojiAnchor] = useState<Anchor | null>(null)
  const [editTarget, setEditTarget] = useState<Message | null>(null)
  const [editText, setEditText] = useState('')
  const [forwardTarget, setForwardTarget] = useState<Message | null>(null)
  const [viewerFile, setViewerFile] = useState<FileRef | null>(null)
  const [dmIds, setDmIds] = useState<string[]>(() => dmMembers.get(channel.id) ?? [])

  const loadingMoreRef = useRef(false)
  const lastAnnouncedRef = useRef<string | null>(null)
  const forwardHeading = useModalFocus(!!forwardTarget)

  const isDM = channel.type === 'dm' || channel.type === 'group_dm'

  const loadMessages = useCallback(async () => {
    try {
      const res = await messageApi.list(channel.id)
      setMessages(channel.id, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
      setLoadError(null)
    } catch (err) {
      setLoadError(errorMessage(err, 'Could not load messages'))
    }
  }, [channel.id, setMessages])

  useEffect(() => {
    loadMessages()
  }, [loadMessages])

  // Optimistic rows belong to the channel that was open when they were queued.
  useEffect(() => {
    setOutbox({})
    setLoadMoreError(null)
    // Otherwise switching channels announces that channel's newest message as
    // if it had just arrived.
    lastAnnouncedRef.current = null
    // The thread lives in the shell now, so it outlives this component unless
    // somebody closes it: switching channels with a thread open would otherwise
    // leave another channel's replies pinned beside the new conversation.
    useUiStore.getState().closeThread()
  }, [channel.id])

  useEffect(() => {
    if (typingIds.length) ensureUsers(typingIds)
  }, [typingIds, ensureUsers])

  // A DM has `name === null`; every one of them rendered the literal
  // "Direct message". The member list is the only place the participants are.
  useEffect(() => {
    if (!isDM || channel.name) return
    const cached = dmMembers.get(channel.id)
    if (cached) {
      setDmIds(cached)
      ensureUsers(cached)
      return
    }
    let cancelled = false
    channelApi
      .listMembers(channel.workspace_id, channel.id)
      .then((res) => {
        const ids = (res.data ?? []).map((m) => m.user_id)
        dmMembers.set(channel.id, ids)
        if (cancelled) return
        setDmIds(ids)
        ensureUsers(ids)
      })
      .catch(() => {
        /* the header falls back to "Direct message" */
      })
    return () => {
      cancelled = true
    }
  }, [channel.id, channel.workspace_id, channel.name, isDM, ensureUsers])

  const title = useMemo(() => {
    if (channel.name) return channel.name
    if (!isDM) return 'Channel'
    const others = dmIds.filter((id) => id !== currentUserId)
    if (others.length === 0) return 'Direct message'
    return others.map((id) => displayName(users, id)).join(', ')
  }, [channel.name, currentUserId, dmIds, isDM, users])

  // --- pagination ----------------------------------------------------------

  const handleLoadMore = useCallback(async () => {
    if (loadingMoreRef.current) return
    const { cursors, hasMore } = useMessageStore.getState()
    if (!hasMore[channel.id]) return
    const cursor = cursors[channel.id]
    // An empty cursor means the live window was trimmed; the channel has to be
    // reloaded from the top rather than paged.
    if (!cursor) return
    loadingMoreRef.current = true
    setLoadingMore(true)
    setLoadMoreError(null)
    try {
      const res = await messageApi.list(channel.id, cursor)
      prependMessages(channel.id, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
    } catch (err) {
      // Swallowing this left the list stuck with no explanation.
      setLoadMoreError(errorMessage(err, 'Could not load older messages'))
    } finally {
      loadingMoreRef.current = false
      setLoadingMore(false)
    }
  }, [channel.id, prependMessages])

  // --- sending -------------------------------------------------------------

  const deliver = useCallback(
    async (tempId: string, content: string, files?: UploadedFile[]) => {
      setOutbox((prev) => ({ ...prev, [tempId]: { status: 'sending', content, files } }))
      try {
        const res = await messageApi.send(
          channel.id,
          content,
          files?.length ? { fileIds: files.map((f) => f.id) } : undefined,
        )
        const store = useMessageStore.getState()
        // Ids differ, so this is a remove + add rather than an update. If the
        // `message.new` frame beat the response, addMessage dedupes on id.
        store.removeMessage(channel.id, tempId)
        store.addMessage(channel.id, res.data)
        setOutbox((prev) => {
          const next = { ...prev }
          delete next[tempId]
          return next
        })
        setScrollKey((k) => k + 1)
      } catch {
        // The row stays in place showing "Not delivered · Retry · Discard"
        // instead of the text being lost behind an Alert.
        setOutbox((prev) => ({
          ...prev,
          [tempId]: { ...prev[tempId], status: 'failed', content, files },
        }))
      }
    },
    [channel.id],
  )

  const handleSend = useCallback(
    (content: string, files?: UploadedFile[]) => {
      if (!content.trim() && !files?.length) return
      const id = newTempId()
      useMessageStore
        .getState()
        .addMessage(
          channel.id,
          // `user_id` is only missing in the brief window before the hydrated
          // profile lands; dropping the text there would be worse.
          optimisticMessage({ id, channelId: channel.id, userId: currentUserId ?? '', content, files }),
        )
      setScrollKey((k) => k + 1)
      void deliver(id, content, files)
    },
    [channel.id, currentUserId, deliver],
  )

  const retrySend = useCallback(
    (m: Message) => {
      const entry = outbox[m.id]
      if (entry) void deliver(m.id, entry.content, entry.files)
    },
    [deliver, outbox],
  )

  const discardSend = useCallback(
    (m: Message) => {
      useMessageStore.getState().removeMessage(channel.id, m.id)
      setOutbox((prev) => {
        const next = { ...prev }
        delete next[m.id]
        return next
      })
    },
    [channel.id],
  )

  const sendStates = useMemo(() => {
    const out: Record<string, SendState> = {}
    Object.entries(outbox).forEach(([id, entry]) => {
      out[id] = entry.status
    })
    return out
  }, [outbox])

  // --- message actions -----------------------------------------------------

  const reactWithEmoji = useCallback(
    async (message: Message, emoji: string) => {
      if (!currentUserId) return
      const apply = useMessageStore.getState().applyReaction
      const optimistic = {
        id: '',
        message_id: message.id,
        user_id: currentUserId,
        emoji,
        created_at: new Date().toISOString(),
      }
      apply(channel.id, optimistic, true)
      try {
        await messageApi.react(channel.id, message.id, emoji)
      } catch (err) {
        apply(channel.id, optimistic, false)
        Alert.alert('Error', errorMessage(err, 'Could not add reaction'))
      }
    },
    [channel.id, currentUserId],
  )

  const doPin = useCallback(
    async (m: Message) => {
      try {
        if (m.is_pinned) await messageApi.unpin(channel.id, m.id)
        else await messageApi.pin(channel.id, m.id)
        useMessageStore.getState().updateMessage(channel.id, { ...m, is_pinned: !m.is_pinned })
      } catch (err) {
        Alert.alert('Error', errorMessage(err, 'Could not pin message'))
      }
    },
    [channel.id],
  )

  const doBookmark = useCallback(async (m: Message) => {
    try {
      await messageApi.bookmark(m.id)
      Alert.alert('Saved', 'Message bookmarked')
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not bookmark message'))
    }
  }, [])

  const doDelete = useCallback(
    (m: Message) => {
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
              Alert.alert('Error', errorMessage(err, 'Could not delete message'))
            }
          },
        },
      ])
    },
    [channel.id],
  )

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
      Alert.alert('Error', errorMessage(err, 'Could not edit message'))
    }
  }

  const doForward = async (m: Message, targetChannelId: string) => {
    try {
      await messageApi.forward(channel.id, m.id, targetChannelId)
      setForwardTarget(null)
      Alert.alert('Forwarded', 'Message forwarded')
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not forward message'))
    }
  }

  const openFile = useCallback((file: FileRef) => {
    if ((file.content_type || '').startsWith('image/')) {
      setViewerFile(file)
      return
    }
    Linking.openURL(fileURL(file.id)).catch(() =>
      Alert.alert('Error', 'Could not open that attachment.'),
    )
  }, [])

  /**
   * The thread is handed to the shell rather than opened here. Whether it is a
   * third column, a sheet over the conversation or a full screen depends on the
   * viewport, and this component cannot see the viewport it is placed in.
   */
  const openThread = useCallback(
    (m: Message) => useUiStore.getState().openThread(channel.id, m),
    [channel.id],
  )

  const addReaction = useCallback((m: Message, anchor?: Anchor) => {
    setEmojiTarget(m)
    setEmojiAnchor(anchor ?? null)
  }, [])

  // Six Alert buttons silently collapsed to three on Android, which is why
  // Forward/Edit/Delete were unreachable there. This is a sheet instead.
  const openActions = useCallback((m: Message) => setActionTarget(m), [])

  const actions = useMemo<SheetAction[]>(() => {
    const m = actionTarget
    if (!m) return []
    const isOwn = m.user_id === currentUserId
    const list: SheetAction[] = [
      {
        key: 'react',
        label: 'Add reaction',
        icon: '😀',
        onPress: () => {
          setEmojiTarget(m)
          setEmojiAnchor(null)
        },
      },
      {
        key: 'thread',
        label: 'Reply in thread',
        icon: '💬',
        onPress: () => useUiStore.getState().openThread(channel.id, m),
      },
      {
        key: 'pin',
        label: m.is_pinned ? 'Unpin from channel' : 'Pin to channel',
        icon: '📌',
        onPress: () => doPin(m),
      },
      { key: 'bookmark', label: 'Save for later', icon: '🔖', onPress: () => doBookmark(m) },
      { key: 'forward', label: 'Forward…', icon: '↗️', onPress: () => setForwardTarget(m) },
    ]
    if (isOwn) {
      list.push({
        key: 'edit',
        label: 'Edit message',
        icon: '✏️',
        onPress: () => {
          setEditTarget(m)
          setEditText(m.content)
        },
      })
      list.push({
        key: 'delete',
        label: 'Delete message',
        icon: '🗑️',
        destructive: true,
        onPress: () => doDelete(m),
      })
    }
    return list
  }, [actionTarget, channel.id, currentUserId, doBookmark, doDelete, doPin])

  // --- derived view state --------------------------------------------------

  // Thread replies are excluded from GET /channels/{id}/messages server-side
  // (`parent_id IS NULL`), but the `message.new` frame for one is not — without
  // this filter a reply appears twice: in its thread and at the channel root.
  const rootMessages = useMemo(() => currentMessages.filter((m) => !m.parent_id), [currentMessages])

  const typingLabel = useMemo(() => {
    const names = typingIds.map((id) => displayName(users, id))
    if (names.length === 0) return null
    if (names.length === 1) return `${names[0]} is typing…`
    if (names.length === 2) return `${names[0]} and ${names[1]} are typing…`
    return 'Several people are typing…'
  }, [typingIds, users])

  // Screen readers get no signal that the list grew. Announce arrivals from
  // other people only — your own send is already confirmed by the composer.
  useEffect(() => {
    const last = rootMessages[rootMessages.length - 1]
    if (!last) return
    if (lastAnnouncedRef.current === null) {
      lastAnnouncedRef.current = last.id
      return
    }
    if (lastAnnouncedRef.current === last.id) return
    lastAnnouncedRef.current = last.id
    if (last.user_id === currentUserId) return
    announce(`${displayName(users, last.user_id)} said ${last.content}`)
  }, [rootMessages, currentUserId, users])

  return (
    <KeyboardAvoidingView style={{ flex: 1 }} behavior={Platform.OS === 'ios' ? 'padding' : undefined}>
      {/* Header */}
      <View
        style={{
          height: twoPane ? 52 : 56,
          paddingLeft: backVisible ? space.sm : gutter,
          paddingRight: twoPane ? space.md : space.sm,
          flexDirection: 'row',
          alignItems: 'center',
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
          backgroundColor: theme.bg,
        }}
      >
        {/* The back button exists to reach a channel list that is not on screen.
            With the sidebar pinned beside this pane it points at something the
            user is already looking at, so it goes. */}
        {backVisible && (
          <Pressable
            onPress={onBack}
            accessibilityRole="button"
            accessibilityLabel="Back to channel list"
            style={{
              width: minTouch,
              height: minTouch,
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 20 }}>←</Text>
          </Pressable>
        )}
        <Text style={{ color: theme.textMuted, marginRight: 4 }}>{isDM ? '💬' : '#'}</Text>
        <Text
          accessibilityRole="header"
          // Shrinks rather than grows: the topic beside it takes the slack, and
          // without this a long channel name would push the header controls off
          // the right edge instead of truncating.
          style={{ color: theme.text, fontWeight: '600', fontSize: 16, flexShrink: 1 }}
          numberOfLines={1}
        >
          {title}
        </Text>
        {/* The topic is the first thing a phone has to drop and the first thing
            a wide header has room for. */}
        {showTopic ? (
          <>
            <View
              style={{
                width: 1,
                height: 16,
                marginHorizontal: space.md,
                backgroundColor: theme.borderStrong,
              }}
            />
            <Text style={{ color: theme.textMuted, fontSize: 13, flex: 1 }} numberOfLines={1}>
              {channel.topic}
            </Text>
          </>
        ) : (
          <View style={{ flex: 1 }} />
        )}
        {onOpenMembers && (
          <Pressable
            onPress={onOpenMembers}
            accessibilityRole="button"
            accessibilityLabel="Channel members"
            style={{
              width: minTouch,
              height: minTouch,
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 18 }}>👥</Text>
          </Pressable>
        )}
      </View>

      {/* Huddles. Renders nothing at all when this deployment has no media
          server — a button that 404s is worse than no button. A DM is a
          channel, so the bar works there too. */}
      <HuddleBar channelId={channel.id} canWrite={!channel.is_archived} />

      {/* Messages */}
      {loadError && rootMessages.length === 0 ? (
        <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center', padding: 24, gap: 12 }}>
          <Text style={{ color: theme.textMuted, fontSize: 15, textAlign: 'center' }}>{loadError}</Text>
          <Pressable
            onPress={loadMessages}
            accessibilityRole="button"
            accessibilityLabel="Retry loading messages"
            style={{
              minHeight: minTouch,
              paddingHorizontal: 20,
              justifyContent: 'center',
              borderRadius: 12,
              backgroundColor: theme.primary,
            }}
          >
            <Text style={{ color: theme.primaryText, fontWeight: '600' }}>Try again</Text>
          </Pressable>
        </View>
      ) : (
        <MessageList
          messages={rootMessages}
          sendStates={sendStates}
          customEmoji={customEmoji}
          onLongPress={openActions}
          onAddReaction={addReaction}
          onOpenThread={openThread}
          onPin={doPin}
          onOpenFile={openFile}
          onRetrySend={retrySend}
          onDiscardSend={discardSend}
          onLoadMore={handleLoadMore}
          loadingMore={loadingMore}
          loadMoreError={loadMoreError}
          scrollToEndKey={scrollKey}
        />
      )}

      {/* Typing indicator */}
      {typingLabel && (
        <View
          accessibilityLiveRegion="polite"
          accessible
          accessibilityLabel={typingLabel}
          style={{ paddingHorizontal: gutter, paddingBottom: 4 }}
        >
          <Text style={{ color: theme.textMuted, fontSize: 12, fontStyle: 'italic' }}>{typingLabel}</Text>
        </View>
      )}

      {/* Input */}
      <MessageInput
        onSend={handleSend}
        channelName={title}
        channelId={channel.id}
        // A DM has no "#name" to prefix.
        placeholder={isDM ? `Message ${title}` : undefined}
      />

      {/* Long-press menu */}
      <ActionSheet
        visible={!!actionTarget}
        title="Message"
        actions={actions}
        onClose={() => setActionTarget(null)}
      />

      {/* Emoji picker (React action / + reaction) */}
      <EmojiPicker
        visible={!!emojiTarget}
        anchor={emojiAnchor}
        onClose={() => {
          setEmojiTarget(null)
          setEmojiAnchor(null)
        }}
        onSelect={(emoji) => {
          if (emojiTarget) reactWithEmoji(emojiTarget, emoji)
        }}
      />

      {/* The thread is NOT rendered here. `openThread` puts it in `uiStore` and
          the shell presents it, because only the shell knows whether there is
          room for a third column. */}

      {/* Full-screen image */}
      <AttachmentViewer file={viewerFile} onClose={() => setViewerFile(null)} />

      {/* Edit modal */}
      <Modal visible={!!editTarget} transparent animationType="fade" onRequestClose={() => setEditTarget(null)}>
        <View
          accessibilityViewIsModal
          style={{ flex: 1, backgroundColor: 'rgba(0,0,0,0.6)', justifyContent: 'center', paddingHorizontal: 24 }}
        >
          {/* A dialog is sized by its content, not by the window: stretched to
              1500px the buttons end up a screen apart from the text. */}
          <View
            style={{
              width: '100%',
              maxWidth: 520,
              alignSelf: 'center',
              backgroundColor: theme.surface,
              borderRadius: 16,
              padding: 16,
              borderWidth: 1,
              borderColor: theme.border,
            }}
          >
            <Text accessibilityRole="header" style={{ color: theme.text, fontSize: 15, fontWeight: '600', marginBottom: 12 }}>
              Edit message
            </Text>
            <TextInput
              value={editText}
              onChangeText={setEditText}
              multiline
              autoFocus
              accessibilityLabel="Message text"
              placeholderTextColor={theme.textMuted}
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
                accessibilityRole="button"
                accessibilityLabel="Cancel editing"
                style={{ paddingHorizontal: 16, minHeight: minTouch, justifyContent: 'center', borderRadius: 10, backgroundColor: theme.surfaceAlt }}
              >
                <Text style={{ color: theme.textMuted, fontWeight: '600' }}>Cancel</Text>
              </Pressable>
              <Pressable
                onPress={submitEdit}
                accessibilityRole="button"
                accessibilityLabel="Save changes"
                style={{ paddingHorizontal: 16, minHeight: minTouch, justifyContent: 'center', borderRadius: 10, backgroundColor: theme.primary }}
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
          accessibilityRole="button"
          accessibilityLabel="Close"
          style={{
            flex: 1,
            backgroundColor: 'rgba(0,0,0,0.6)',
            // A sheet slides in from the thumb's edge of a phone. With a pointer
            // there is no such edge, and a centred dialog is the idiom.
            justifyContent: twoPane ? 'center' : 'flex-end',
            alignItems: twoPane ? 'center' : 'stretch',
          }}
        >
          <Pressable
            onPress={(e) => e.stopPropagation()}
            accessibilityViewIsModal
            style={{
              backgroundColor: theme.surface,
              borderTopLeftRadius: 20,
              borderTopRightRadius: 20,
              borderBottomLeftRadius: twoPane ? 20 : 0,
              borderBottomRightRadius: twoPane ? 20 : 0,
              width: twoPane ? 420 : undefined,
              maxWidth: '100%',
              paddingHorizontal: 16,
              paddingTop: 16,
              paddingBottom: twoPane ? 16 : 28,
              borderTopWidth: 1,
              borderWidth: twoPane ? 1 : 0,
              borderColor: theme.border,
              maxHeight: '70%',
            }}
          >
            <View {...forwardHeading} accessible accessibilityRole="header" style={{ marginBottom: 12 }}>
              <Text style={{ color: theme.text, fontSize: 15, fontWeight: '600' }}>Forward to…</Text>
            </View>
            <ScrollView>
              {channels
                .filter((c) => c.id !== channel.id && !c.is_archived)
                .map((c) => (
                  <Pressable
                    key={c.id}
                    onPress={() => forwardTarget && doForward(forwardTarget, c.id)}
                    accessibilityRole="button"
                    accessibilityLabel={`Forward to ${c.name || 'direct message'}`}
                    style={{ flexDirection: 'row', alignItems: 'center', minHeight: minTouch }}
                  >
                    <Text style={{ color: theme.textMuted, marginRight: 6 }}>
                      {c.type === 'dm' || c.type === 'group_dm' ? '💬' : '#'}
                    </Text>
                    <Text style={{ color: theme.body, fontSize: 15 }}>{c.name || 'Direct message'}</Text>
                  </Pressable>
                ))}
            </ScrollView>
          </Pressable>
        </Pressable>
      </Modal>
    </KeyboardAvoidingView>
  )
}
