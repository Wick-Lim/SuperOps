import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { Alert, KeyboardAvoidingView, Linking, Platform, Pressable, Text, View } from 'react-native'
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
import { space, useResponsive } from '../../lib/responsive'
import { useModalFocus } from '../a11y'
import MessageList from './MessageList'
import MessageInput from './MessageInput'
import EmojiPicker from './EmojiPicker'
import AttachmentViewer from './AttachmentViewer'
import type { SendState } from './MessageItem'
import type { Anchor } from './anchor'
import { newTempId, optimisticMessage, type OutboxEntry } from './outbox'

interface Props {
  /**
   * Defaults to true. The shell mounts this only when a thread is open, so the
   * flag exists for a parent that would rather keep it mounted and hidden.
   */
  visible?: boolean
  root: Message | null
  channelId: string
  channelName?: string
  customEmoji?: CustomEmoji[]
  onClose: () => void
  /**
   * Called only when the server does NOT rebroadcast the parent. Optional: with
   * no handler the reply count is bumped in `messageStore` directly, so a shell
   * that renders this as a pane does not have to know the rule.
   */
  onReplied?: (rootId: string) => void
  /**
   * Whether to name the channel under the "Thread" title. Defaults to false in a
   * three-pane layout, where the channel header is already on screen two inches
   * to the left and repeating it is just noise.
   */
  showChannelContext?: boolean
}

const PAGE = 50
const EMPTY: Message[] = []

/**
 * Thread view.
 *
 * `GET /messages/{id}/thread` is ascending and pages FORWARD (the cursor is
 * keyed on `created_at` with `>`), so "load more" lives at the bottom, not the
 * top — hence `paginate="end"`.
 *
 * This renders a plain column that fills whatever box it is given. It used to be
 * a `Modal` that owned its own full-screen presentation, which made it the one
 * thing on screen no matter how much room there was. Who presents it — a third
 * column beside the conversation, a sheet over it, or a full screen — is a
 * layout decision, and only the shell knows the viewport, so the shell decides.
 */
export default function ThreadView({
  visible = true,
  root,
  channelId,
  channelName,
  customEmoji,
  onClose,
  onReplied,
  showChannelContext,
}: Props) {
  const currentUserId = useAuthStore((s) => s.user?.id)
  const { tier, minTouch, threePane } = useResponsive()
  const [replies, setReplies] = useState<Message[]>([])
  // Own picker and viewer: a Modal opened from ChannelView's tree would be
  // presented *under* this one.
  const [emojiTarget, setEmojiTarget] = useState<Message | null>(null)
  const [emojiAnchor, setEmojiAnchor] = useState<Anchor | null>(null)
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

  const pointer = tier !== 'compact'
  const withContext = showChannelContext ?? !threePane
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

  /**
   * The parent's `reply_count` only needs bumping locally when the server did
   * not rebroadcast it (see `deliver`). A caller can override this, but the
   * default keeps the rule with the code that knows it.
   */
  const bumpReplyCount = useCallback(
    (id: string) => {
      if (onReplied) {
        onReplied(id)
        return
      }
      const store = useMessageStore.getState()
      const current = (store.messages[channelId] ?? EMPTY).find((m) => m.id === id)
      if (current) store.updateMessage(channelId, { ...current, reply_count: current.reply_count + 1 })
    },
    [channelId, onReplied],
  )

  const deliver = useCallback(
    async (tempId: string, content: string, files?: UploadedFile[]) => {
      if (!rootId) return
      setOutbox((prev) => ({ ...prev, [tempId]: { status: 'sending', content, files } }))
      try {
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
        if (withFiles) bumpReplyCount(rootId)
        setScrollKey((k) => k + 1)
      } catch (err) {
        setOutbox((prev) => ({
          ...prev,
          [tempId]: { ...prev[tempId], status: 'failed', content, files },
        }))
        setError(errorMessage(err, 'Could not send reply'))
      }
    },
    [bumpReplyCount, channelId, rootId],
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

  const addReaction = useCallback((m: Message, anchor?: Anchor) => {
    setEmojiTarget(m)
    setEmojiAnchor(anchor ?? null)
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
  const countLabel = `${replyCount} ${replyCount === 1 ? 'reply' : 'replies'}`

  if (!visible || !root) return null

  const close = (
    <Pressable
      onPress={onClose}
      accessibilityRole="button"
      accessibilityLabel="Close thread"
      style={{
        width: minTouch,
        height: minTouch,
        alignItems: 'center',
        justifyContent: 'center',
      }}
    >
      <Text style={{ color: theme.textMuted, fontSize: pointer ? 17 : 20 }}>✕</Text>
    </Pressable>
  )

  return (
    <View style={{ flex: 1, backgroundColor: theme.bg }}>
      <KeyboardAvoidingView
        style={{ flex: 1 }}
        behavior={Platform.OS === 'ios' ? 'padding' : undefined}
      >
        <View
          style={{
            height: pointer ? 48 : 56,
            paddingHorizontal: space.md,
            flexDirection: 'row',
            alignItems: 'center',
            gap: space.sm,
            borderBottomWidth: 1,
            borderBottomColor: theme.border,
          }}
        >
          {/* Full screen, ✕ reads as "go back" and belongs where a back button
              would be. In a column beside the conversation it is closing that
              column, and a close control belongs at its trailing edge. */}
          {!pointer && close}
          <View {...heading} accessible accessibilityRole="header" style={{ flex: 1 }}>
            <Text style={{ color: theme.text, fontWeight: '600', fontSize: pointer ? 15 : 16 }}>
              Thread
            </Text>
            <Text style={{ color: theme.textMuted, fontSize: 12 }} numberOfLines={1}>
              {withContext && channelName ? `#${channelName} · ${countLabel}` : countLabel}
            </Text>
          </View>
          {pointer && close}
        </View>

        <MessageList
          messages={messages}
          sendStates={sendStates}
          customEmoji={customEmoji}
          onAddReaction={addReaction}
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
          // The pane is ~380px wide however big the window is, so it keeps the
          // phone's gutter rather than the desktop one.
          gutter={pointer ? space.lg : undefined}
        />

        <MessageInput
          onSend={handleSend}
          channelId={channelId}
          channelName={channelName}
          placeholder="Reply to thread…"
        />

        <EmojiPicker
          visible={!!emojiTarget}
          anchor={emojiAnchor}
          onClose={() => {
            setEmojiTarget(null)
            setEmojiAnchor(null)
          }}
          onSelect={(emoji) => {
            if (emojiTarget) void toggleReaction(emojiTarget, emoji, false)
          }}
        />
        <AttachmentViewer file={viewerFile} onClose={() => setViewerFile(null)} />
      </KeyboardAvoidingView>
    </View>
  )
}
