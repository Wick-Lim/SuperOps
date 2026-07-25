import React, { useCallback, useEffect, useMemo, useRef } from 'react'
import { ActivityIndicator, Pressable, Text, View } from 'react-native'
import { FlashList, type FlashListRef, type ListRenderItemInfo } from '@shopify/flash-list'
import type { FileRef, Message } from '../../lib/types'
import { theme } from '../../lib/theme'
import { useUserStore } from '../../stores/userStore'
import type { CustomEmoji } from '../../api/emoji'
import { space, useResponsive } from '../../lib/responsive'
import MessageItem, { type SendState } from './MessageItem'
import type { Anchor } from './anchor'
import { dayKey, formatDayLabel } from './time'

/** One rendered row: the message plus everything derived from its neighbour. */
interface Row {
  message: Message
  showHeader: boolean
  dayLabel: string | null
}

interface Props {
  /** Ascending (oldest first), as `messageStore` keeps them. */
  messages: Message[]
  sendStates?: Record<string, SendState>
  customEmoji?: CustomEmoji[]
  onLongPress?: (message: Message) => void
  onAddReaction?: (message: Message, anchor?: Anchor) => void
  onToggleReaction?: (message: Message, emoji: string, mine: boolean) => void
  onOpenThread?: (message: Message) => void
  /** Enables the pin button in the pointer-only hover toolbar. */
  onPin?: (message: Message) => void
  onOpenFile?: (file: FileRef) => void
  onRetrySend?: (message: Message) => void
  onDiscardSend?: (message: Message) => void
  onLoadMore?: () => void
  /** Retry for the inline error; falls back to `onLoadMore`. */
  onRetryLoadMore?: () => void
  loadingMore?: boolean
  /** Set when the last page fetch failed; renders an inline retry. */
  loadMoreError?: string | null
  /**
   * Which edge pages. Channels page backwards (older messages at the top);
   * threads page forwards (`GET /messages/{id}/thread` is ascending).
   */
  paginate?: 'start' | 'end'
  emptyText?: string
  /** Bump to force a scroll to the newest row (e.g. after sending). */
  scrollToEndKey?: number
  /**
   * Overrides the tier's page padding. `useResponsive` measures the WINDOW, so
   * a list inside a 380px thread pane on a 1600px monitor would otherwise be
   * given the desktop gutter and lose an eighth of its width to it.
   */
  gutter?: number
}

const GROUP_WINDOW_MS = 300_000
const EMPTY_STATES: Record<string, SendState> = {}

function buildRows(messages: Message[]): Row[] {
  const rows: Row[] = new Array(messages.length)
  for (let i = 0; i < messages.length; i++) {
    const m = messages[i]
    const prev = i > 0 ? messages[i - 1] : null
    const newDay = !prev || dayKey(prev.created_at) !== dayKey(m.created_at)
    rows[i] = {
      message: m,
      dayLabel: newDay ? formatDayLabel(m.created_at) : null,
      showHeader:
        newDay ||
        !prev ||
        prev.user_id !== m.user_id ||
        new Date(m.created_at).getTime() - new Date(prev.created_at).getTime() > GROUP_WINDOW_MS,
    }
  }
  return rows
}

export default function MessageList({
  messages,
  sendStates = EMPTY_STATES,
  customEmoji,
  onLongPress,
  onAddReaction,
  onToggleReaction,
  onOpenThread,
  onPin,
  onOpenFile,
  onRetrySend,
  onDiscardSend,
  onLoadMore,
  onRetryLoadMore,
  loadingMore,
  loadMoreError,
  paginate = 'start',
  emptyText = 'No messages yet. Start the conversation!',
  scrollToEndKey = 0,
  gutter,
}: Props) {
  const listRef = useRef<FlashListRef<Row>>(null)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  // Resolved once here and handed to every row: fifty rows each calling
  // `useResponsive` would mean fifty window-dimension subscriptions per screen.
  const { tier, minTouch, gutter: tierGutter } = useResponsive()
  const pad = gutter ?? tierGutter

  const rows = useMemo(() => buildRows(messages), [messages])

  // One batched resolve for the whole page. Every row used to run its own
  // `ensureUsers` effect, so mounting 50 rows queued 50 effect passes.
  useEffect(() => {
    const ids = Array.from(new Set(messages.map((m) => m.user_id).filter(Boolean)))
    if (ids.length) ensureUsers(ids)
  }, [messages, ensureUsers])

  useEffect(() => {
    if (scrollToEndKey > 0) listRef.current?.scrollToEnd({ animated: true })
  }, [scrollToEndKey])

  // `extraData` is how FlashList learns that a closure the rows read from
  // changed; the row components themselves are memoized on their props.
  const extraData = useMemo(
    () => ({ sendStates, customEmoji, tier, minTouch }),
    [sendStates, customEmoji, tier, minTouch],
  )

  // Dragging a window across a breakpoint changes this, and FlashList needs a
  // new object to notice.
  const contentStyle = useMemo(
    () => ({ paddingHorizontal: pad, paddingVertical: tier === 'compact' ? space.sm : space.md }),
    [pad, tier],
  )

  const renderItem = useCallback(
    ({ item }: ListRenderItemInfo<Row>) => (
      <MessageItem
        message={item.message}
        showHeader={item.showHeader}
        dayLabel={item.dayLabel}
        sendState={sendStates[item.message.id]}
        customEmoji={customEmoji}
        tier={tier}
        minTouch={minTouch}
        onLongPress={onLongPress}
        onAddReaction={onAddReaction}
        onToggleReaction={onToggleReaction}
        onOpenThread={onOpenThread}
        onPin={onPin}
        onOpenFile={onOpenFile}
        onRetrySend={onRetrySend}
        onDiscardSend={onDiscardSend}
      />
    ),
    [
      sendStates,
      customEmoji,
      tier,
      minTouch,
      onLongPress,
      onAddReaction,
      onToggleReaction,
      onOpenThread,
      onPin,
      onOpenFile,
      onRetrySend,
      onDiscardSend,
    ],
  )

  const keyExtractor = useCallback((row: Row) => row.message.id, [])

  // Recycling works best when visually different rows are typed apart.
  const getItemType = useCallback(
    (row: Row) => (row.dayLabel ? 'day' : row.showHeader ? 'header' : 'compact'),
    [],
  )

  const pager = !loadMoreError ? (
    loadingMore ? (
      <View style={{ paddingVertical: 12, alignItems: 'center' }}>
        <ActivityIndicator size="small" color={theme.textMuted} />
      </View>
    ) : null
  ) : (
    <View style={{ paddingVertical: 12, alignItems: 'center', gap: 6 }}>
      <Text style={{ color: theme.textMuted, fontSize: 12 }}>{loadMoreError}</Text>
      <Pressable
        onPress={onRetryLoadMore ?? onLoadMore}
        accessibilityRole="button"
        accessibilityLabel="Retry loading more messages"
        style={{
          paddingHorizontal: 14,
          paddingVertical: 8,
          borderRadius: 10,
          borderWidth: 1,
          borderColor: theme.borderStrong,
          backgroundColor: theme.surface,
        }}
      >
        <Text style={{ color: theme.accent, fontSize: 13, fontWeight: '600' }}>Try again</Text>
      </Pressable>
    </View>
  )

  if (rows.length === 0) {
    return (
      <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center', padding: 24, gap: 12 }}>
        {loadingMore ? <ActivityIndicator size="small" color={theme.textMuted} /> : null}
        <Text style={{ color: theme.textMuted, fontSize: 15, textAlign: 'center' }}>{emptyText}</Text>
        {loadMoreError ? pager : null}
      </View>
    )
  }

  const pagesBackwards = paginate === 'start'

  return (
    <FlashList
      ref={listRef}
      data={rows}
      extraData={extraData}
      keyExtractor={keyExtractor}
      getItemType={getItemType}
      renderItem={renderItem}
      // FlashList's outer container is a plain View, so unlike a FlatList
      // (whose ScrollView flex-grows by default) it collapses to zero height
      // without this.
      style={{ flex: 1 }}
      contentContainerStyle={contentStyle}
      ListHeaderComponent={pagesBackwards ? pager : null}
      ListFooterComponent={pagesBackwards ? null : pager}
      onStartReached={pagesBackwards ? onLoadMore : undefined}
      onStartReachedThreshold={0.3}
      onEndReached={pagesBackwards ? undefined : onLoadMore}
      onEndReachedThreshold={0.3}
      /**
       * The anchoring fix. Older pages are spliced in at index 0; without this
       * the viewport stayed at the same offset and therefore jumped to an
       * arbitrary older message on every page load. FlashList v2 pins the first
       * visible item instead, and still follows new arrivals at the bottom when
       * the reader is already there.
       */
      maintainVisibleContentPosition={{
        autoscrollToBottomThreshold: 0.2,
        startRenderingFromBottom: pagesBackwards,
      }}
    />
  )
}
