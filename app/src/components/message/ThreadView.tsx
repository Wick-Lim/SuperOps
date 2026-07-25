import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  Alert,
  KeyboardAvoidingView,
  Linking,
  Modal,
  Platform,
  Pressable,
  SafeAreaView,
  Text,
  View,
} from 'react-native'
import { messageApi } from '../../api/messages'
import { errorMessage, fileURL } from '../../api/client'
import type { UploadedFile } from '../../api/files'
import type { CustomEmoji } from '../../api/emoji'
import { useAuthStore } from '../../stores/authStore'
import { useMessageStore } from '../../stores/messageStore'
import { wsManager } from '../../lib/websocket'
import { normalizeMessage } from '../../lib/types'
import type { FileRef, Message, Reaction, WireMessage } from '../../lib/types'
import { theme } from '../../lib/theme'
import { MIN_TOUCH, useModalFocus } from '../a11y'
import MessageList from './MessageList'
import MessageInput from './MessageInput'
import EmojiPicker from './EmojiPicker'
import AttachmentViewer from './AttachmentViewer'
import type { SendState } from './MessageItem'
import { newTempId, optimisticMessage, type OutboxEntry } from './outbox'

interface Props {
  visible: boolean
  root: Message | null
  channelId: string
  channelName?: string
  customEmoji?: CustomEmoji[]
  onClose: () => void
  /**
   * Called only when the server does NOT rebroadcast the parent, so the channel
   * can bump reply_count itself without double counting.
   */
  onReplied?: (rootId: string) => void
}

const PAGE = 50

/**
 * Thread view.
 *
 * `GET /messages/{id}/thread` is ascending and pages FORWARD (the cursor is
 * keyed on `created_at` with `>`), so "load more" lives at the bottom, not the
 * top — hence `paginate="end"`.
 *
 * Per this file's convention a channel sub-view is a Modal inside the channel,
 * not a route.
 */
export default function ThreadView({
  visible,
  root,
  channelId,
  channelName,
  customEmoji,
  onClose,
  onReplied,
}: Props) {
  const currentUserId = useAuthStore((s) => s.user?.id)
  const [replies, setReplies] = useState<Message[]>([])
  // Own picker and viewer: a Modal opened from ChannelView's tree would be
  // presented *under* this one.
  const [emojiTarget, setEmojiTarget] = useState<Message | null>(null)
  const [viewerFile, setViewerFile] = useState<FileRef | null>(null)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [outbox, setOutbox] = useState<Record<string, OutboxEntry>>({})
  const [scrollKey, setScrollKey] = useState(0)
  /** Bumped to re-run the first-page load after it failed. */
  const [reloadKey, setReloadKey] = useState(0)
  const loadingRef = useRef(false)
  const heading = useModalFocus(visible)

  const rootId = root?.id

  useEffect(() => {
    if (!visible || !rootId) return
    let cancelled = false
    setReplies([])
    setCursor(undefined)
    setHasMore(false)
    setOutbox({})
    setError(null)
    setLoading(true)
    messageApi
      .listThread(rootId, undefined, PAGE)
      .then((res) => {
        if (cancelled) return
        setReplies(res.data ?? [])
        setCursor(res.meta?.cursor)
        setHasMore(res.meta?.has_more ?? false)
      })
      .catch((err) => {
        if (!cancelled) setError(errorMessage(err, 'Could not load replies'))
      })
      .finally(() => {
        if (!cancelled) setLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [visible, rootId, reloadKey])

  // Live replies. Thread replies are excluded from the channel list server-side
  // (`parent_id IS NULL`), so this view is the only place they belong.
  useEffect(() => {
    if (!visible || !rootId) return
    const upsert = (m: Message) =>
      setReplies((prev) => {
        const idx = prev.findIndex((r) => r.id === m.id)
        if (idx === -1) return [...prev, m]
        const next = prev.slice()
        next[idx] = m
        return next
      })

    const offNew = wsManager.on('message.new', (data) => {
      const m = normalizeMessage(data as WireMessage)
      if (m.parent_id === rootId) upsert(m)
    })
    const offUpdated = wsManager.on('message.updated', (data) => {
      const m = normalizeMessage(data as WireMessage)
      if (m.parent_id === rootId) upsert(m)
    })
    const offDeleted = wsManager.on('message.deleted', (data) => {
      const d = data as { id?: string; message_id?: string } | null
      const id = d?.id ?? d?.message_id
      if (id) setReplies((prev) => prev.filter((r) => r.id !== id))
    })
    return () => {
      offNew()
      offUpdated()
      offDeleted()
    }
  }, [visible, rootId])

  const loadMore = useCallback(async () => {
    if (loadingRef.current || !hasMore || !cursor || !rootId) return
    loadingRef.current = true
    setLoading(true)
    setError(null)
    try {
      const res = await messageApi.listThread(rootId, cursor, PAGE)
      const page = res.data ?? []
      setReplies((prev) => {
        const seen = new Set(prev.map((r) => r.id))
        return [...prev, ...page.filter((m) => !seen.has(m.id))]
      })
      setCursor(res.meta?.cursor)
      setHasMore(res.meta?.has_more ?? false)
    } catch (err) {
      setError(errorMessage(err, 'Could not load more replies'))
    } finally {
      loadingRef.current = false
      setLoading(false)
    }
  }, [cursor, hasMore, rootId])

  const deliver = useCallback(
    async (tempId: string, content: string, files?: UploadedFile[]) => {
      if (!rootId) return
      setOutbox((prev) => ({ ...prev, [tempId]: { status: 'sending', content, files } }))
      try {
        // `POST /messages/{id}/thread` also rebroadcasts the parent so every
        // client's reply_count updates; `POST /channels/.../messages` with a
        // parent_id does not. Files are only accepted by the latter.
        // `POST /messages/{id}/thread` rebroadcasts the hydrated parent, so
        // every client's reply_count updates by itself; `POST
        // /channels/.../messages` with a parent_id does not, but it is the only
        // one that accepts file_ids.
        const withFiles = !!files?.length
        const res = withFiles
          ? await messageApi.send(channelId, content, { parentId: rootId, fileIds: files!.map((f) => f.id) })
          : await messageApi.replyThread(rootId, content)
        setReplies((prev) => {
          const withoutTemp = prev.filter((r) => r.id !== tempId)
          return withoutTemp.some((r) => r.id === res.data.id) ? withoutTemp : [...withoutTemp, res.data]
        })
        setOutbox((prev) => {
          const next = { ...prev }
          delete next[tempId]
          return next
        })
        if (withFiles) onReplied?.(rootId)
        setScrollKey((k) => k + 1)
      } catch (err) {
        setOutbox((prev) => ({
          ...prev,
          [tempId]: { ...prev[tempId], status: 'failed', content, files },
        }))
        setError(errorMessage(err, 'Could not send reply'))
      }
    },
    [channelId, onReplied, rootId],
  )

  const handleSend = useCallback(
    (content: string, files?: UploadedFile[]) => {
      if (!rootId) return
      const id = newTempId()
      setReplies((prev) => [
        ...prev,
        optimisticMessage({
          id,
          channelId,
          userId: currentUserId ?? '',
          content,
          files,
          parentId: rootId,
        }),
      ])
      setScrollKey((k) => k + 1)
      void deliver(id, content, files)
    },
    [channelId, currentUserId, deliver, rootId],
  )

  const retrySend = useCallback(
    (m: Message) => {
      const entry = outbox[m.id]
      if (entry) void deliver(m.id, entry.content, entry.files)
    },
    [deliver, outbox],
  )

  /**
   * The inline "Try again" has to distinguish the two failures: a failed page
   * fetch resumes from the cursor, a failed FIRST load has no cursor to resume
   * from and must re-run the open effect.
   */
  const retryLoad = useCallback(() => {
    if (cursor) void loadMore()
    else setReloadKey((k) => k + 1)
  }, [cursor, loadMore])

  /**
   * Replies live in this component, not in `messageStore` — the channel list
   * excludes them (`parent_id IS NULL`), so the store has no home for them. The
   * root message is the exception: it IS a channel message.
   */
  const toggleReaction = useCallback(
    async (m: Message, emoji: string, mine: boolean) => {
      if (!currentUserId) return
      const optimistic: Reaction = {
        id: '',
        message_id: m.id,
        user_id: currentUserId,
        emoji,
        created_at: new Date().toISOString(),
      }
      const apply = (added: boolean) => {
        if (m.id === rootId) {
          useMessageStore.getState().applyReaction(channelId, optimistic, added)
          return
        }
        setReplies((prev) => {
          const idx = prev.findIndex((r) => r.id === m.id)
          if (idx === -1) return prev
          const target = prev[idx]
          const reactions = (target.reactions ?? []).filter(
            (x) => !(x.user_id === optimistic.user_id && x.emoji === optimistic.emoji),
          )
          if (added) reactions.push(optimistic)
          const next = prev.slice()
          next[idx] = { ...target, reactions }
          return next
        })
      }

      apply(!mine)
      try {
        if (mine) await messageApi.unreact(channelId, m.id, emoji)
        else await messageApi.react(channelId, m.id, emoji)
      } catch (err) {
        apply(mine)
        Alert.alert('Error', errorMessage(err, 'Could not update reaction'))
      }
    },
    [channelId, currentUserId, rootId],
  )

  const openFile = useCallback((file: FileRef) => {
    if ((file.content_type || '').startsWith('image/')) {
      setViewerFile(file)
      return
    }
    Linking.openURL(fileURL(file.id)).catch(() =>
      Alert.alert('Error', 'Could not open that attachment.'),
    )
  }, [])

  const discardSend = useCallback((m: Message) => {
    setReplies((prev) => prev.filter((r) => r.id !== m.id))
    setOutbox((prev) => {
      const next = { ...prev }
      delete next[m.id]
      return next
    })
  }, [])

  const sendStates = useMemo(() => {
    const out: Record<string, SendState> = {}
    Object.entries(outbox).forEach(([id, entry]) => {
      out[id] = entry.status
    })
    return out
  }, [outbox])

  const messages = useMemo(() => (root ? [root, ...replies] : replies), [root, replies])
  const replyCount = replies.length

  return (
    <Modal visible={visible && !!root} animationType="slide" onRequestClose={onClose}>
      <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
        <KeyboardAvoidingView
          style={{ flex: 1 }}
          behavior={Platform.OS === 'ios' ? 'padding' : undefined}
        >
          <View
            style={{
              height: 56,
              paddingHorizontal: 12,
              flexDirection: 'row',
              alignItems: 'center',
              borderBottomWidth: 1,
              borderBottomColor: theme.border,
            }}
          >
            <Pressable
              onPress={onClose}
              accessibilityRole="button"
              accessibilityLabel="Close thread"
              style={{
                width: MIN_TOUCH,
                height: MIN_TOUCH,
                alignItems: 'center',
                justifyContent: 'center',
                marginRight: 4,
              }}
            >
              <Text style={{ color: theme.textMuted, fontSize: 20 }}>✕</Text>
            </Pressable>
            <View {...heading} accessible accessibilityRole="header" style={{ flex: 1 }}>
              <Text style={{ color: theme.text, fontWeight: '600', fontSize: 16 }}>Thread</Text>
              {!!channelName && (
                <Text style={{ color: theme.textMuted, fontSize: 12 }} numberOfLines={1}>
                  #{channelName} · {replyCount} {replyCount === 1 ? 'reply' : 'replies'}
                </Text>
              )}
            </View>
          </View>

          <MessageList
            messages={messages}
            sendStates={sendStates}
            customEmoji={customEmoji}
            onAddReaction={setEmojiTarget}
            onToggleReaction={toggleReaction}
            onOpenFile={openFile}
            onRetrySend={retrySend}
            onDiscardSend={discardSend}
            onLoadMore={loadMore}
            onRetryLoadMore={retryLoad}
            loadingMore={loading}
            loadMoreError={error}
            paginate="end"
            emptyText="No replies yet."
            scrollToEndKey={scrollKey}
          />

          <MessageInput
            onSend={handleSend}
            channelId={channelId}
            channelName={channelName}
            placeholder="Reply to thread…"
          />

          <EmojiPicker
            visible={!!emojiTarget}
            onClose={() => setEmojiTarget(null)}
            onSelect={(emoji) => {
              if (emojiTarget) void toggleReaction(emojiTarget, emoji, false)
            }}
          />
          <AttachmentViewer file={viewerFile} onClose={() => setViewerFile(null)} />
        </KeyboardAvoidingView>
      </SafeAreaView>
    </Modal>
  )
}
