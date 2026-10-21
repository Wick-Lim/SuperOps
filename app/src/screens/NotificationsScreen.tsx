import React, { useCallback, useEffect, useState } from 'react'
import { View, Text, Pressable, FlatList, SafeAreaView, Alert, RefreshControl } from 'react-native'
import { theme } from '../lib/theme'
import {
  notificationApi,
  type InboxEvent,
  type InboxItem,
  type InboxState,
  type NotificationPrefRow,
} from '../api/notifications'
import { channelApi } from '../api/channels'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUiStore } from '../stores/uiStore'
import { errorMessage } from '../api/client'
import type { NotificationType } from '../lib/types'
import { useResponsive } from '../lib/responsive'
import {
  Chip,
  CONTENT_MAX_WIDTH,
  EmptyState,
  ErrorState,
  ListFooter,
  LoadingState,
  ScreenHeader,
  contentColumn,
} from './internal/ui'

const PAGE_SIZE = 30

const ICON_BY_TYPE: Record<NotificationType, string> = {
  mention: '@',
  dm: '💬',
  thread_reply: '↩',
  channel_invite: '#',
  system: '★',
}

const LABEL_BY_TYPE: Record<NotificationType, string> = {
  mention: 'Mention',
  dm: 'Direct message',
  thread_reply: 'Thread reply',
  channel_invite: 'Channel invite',
  system: 'System',
}

function kindToLegacyType(kind: string): NotificationType {
  switch (kind) {
    case 'message.mention':
      return 'mention'
    case 'message.dm':
      return 'dm'
    case 'message.thread_reply':
      return 'thread_reply'
    case 'channel.invited':
      return 'channel_invite'
    default:
      return 'system'
  }
}

function relativeTime(dateStr: string): string {
  const then = new Date(dateStr).getTime()
  if (Number.isNaN(then)) return ''
  const diff = Math.max(0, Date.now() - then)
  const sec = Math.floor(diff / 1000)
  if (sec < 60) return 'just now'
  const min = Math.floor(sec / 60)
  if (min < 60) return `${min}m ago`
  const hr = Math.floor(min / 60)
  if (hr < 24) return `${hr}h ago`
  const day = Math.floor(hr / 24)
  if (day < 7) return `${day}d ago`
  return new Date(dateStr).toLocaleDateString()
}

/**
 * Reads the deep-link target out of a notification's `data`.
 *
 * BOTH SHAPES, because the field changed under this screen. It used to be a
 * JSON string — the repository's `data::text` cast — and the inbox compat layer
 * now emits a real object. `JSON.parse` on an object coerces it to
 * "[object Object]" and throws, so this returned null for every notification
 * and tapping one marked it read and navigated nowhere. The server's own
 * comment claimed "the client parses it as an object either way"; it did not.
 *
 * Accepting both is the right shape regardless: a client in the field is older
 * than the server it talks to, and this is exactly the kind of field that gets
 * re-encoded.
 */
function parseChannelId(data: unknown): string | null {
  const obj =
    typeof data === 'string'
      ? (() => {
          try {
            return JSON.parse(data) as unknown
          } catch {
            return null
          }
        })()
      : data
  if (!obj || typeof obj !== 'object') return null
  const id = (obj as Record<string, unknown>).channel_id
  return typeof id === 'string' && id !== '' ? id : null
}

/** Exported for the test that pins both encodings; not part of the screen's API. */
export const __parseChannelId = parseChannelId

const DEFAULT_PREF_KINDS = [
  '*',
  'message.mention',
  'message.dm',
  'message.thread_reply',
  'issue.assigned',
  'comment.created',
] as const

export default function NotificationsScreen({ navigation }: { navigation: any; route: any }) {
  const channels = useChannelStore((s) => s.channels)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const { tier, gutter, minTouch } = useResponsive()

  const [items, setItems] = useState<InboxItem[]>([])
  const [stateFilter, setStateFilter] = useState<InboxState | undefined>(undefined)
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [expandedId, setExpandedId] = useState<string | null>(null)
  const [eventsByItem, setEventsByItem] = useState<Record<string, InboxEvent[]>>({})
  const [showPrefs, setShowPrefs] = useState(false)
  const [prefs, setPrefs] = useState<NotificationPrefRow[]>([])

  const refreshUnread = useCallback(async () => {
    try {
      const res = await notificationApi.inboxCount()
      useUiStore.getState().setUnread(res.data?.unread ?? 0)
    } catch {
      /* the badge stays as it was */
    }
  }, [])

  const loadPrefs = useCallback(async () => {
    if (!activeWorkspace?.id) return
    try {
      const res = await notificationApi.listPrefs(activeWorkspace.id)
      setPrefs(res.data?.prefs ?? [])
    } catch {
      /* optional panel */
    }
  }, [activeWorkspace?.id])

  /** `next` pages with the cursor the API has always returned and nothing read. */
  const load = useCallback(
    async (mode: 'initial' | 'refresh' | 'next', filterOverride?: InboxState | null) => {
      if (mode === 'initial') setLoading(true)
      if (mode === 'refresh') setRefreshing(true)
      if (mode === 'next') setLoadingMore(true)
      if (mode !== 'next') setError(null)
      const effectiveState = filterOverride === undefined ? stateFilter : (filterOverride ?? undefined)
      try {
        const pageCursor = mode === 'next' ? cursor : undefined
        const res = await notificationApi.inbox({
          state: effectiveState,
          workspace_id: activeWorkspace?.id,
          cursor: pageCursor,
          limit: PAGE_SIZE,
        })
        const page = res.data ?? []
        setItems((prev) => (mode === 'next' ? [...prev, ...page] : page))
        setCursor(res.meta?.cursor)
        setHasMore(!!res.meta?.has_more && !!res.meta?.cursor)
      } catch (err) {
        if (mode === 'next') Alert.alert('Error', errorMessage(err, 'Could not load more'))
        else {
          setError(errorMessage(err, 'Failed to load notifications'))
          setItems([])
        }
      } finally {
        setLoading(false)
        setRefreshing(false)
        setLoadingMore(false)
      }
    },
    [cursor, stateFilter, activeWorkspace?.id],
  )

  useEffect(() => {
    void load('initial')
    void refreshUnread()
    void loadPrefs()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [stateFilter, activeWorkspace?.id])

  const openChannel = async (channelId: string) => {
    const ch = channels.find((c) => c.id === channelId)
    if (ch) {
      useChannelStore.getState().setActiveChannel(ch)
      navigation.navigate('Workspace', {})
      return
    }
    if (!activeWorkspace) return
    try {
      const res = await channelApi.get(activeWorkspace.id, channelId)
      useChannelStore.getState().addChannel(res.data)
      useChannelStore.getState().setActiveChannel(res.data)
      navigation.navigate('Workspace', {})
    } catch (err) {
      Alert.alert('Unavailable', errorMessage(err, 'That conversation is no longer available.'))
    }
  }

  const handleTap = async (item: InboxItem) => {
    if (item.state === 'unread') {
      setItems((prev) => prev.map((x) => (x.id === item.id ? { ...x, state: 'read', unread_count: 0 } : x)))
      try {
        await notificationApi.markRead(item.id)
        void refreshUnread()
      } catch {
        /* optimistic update stands */
      }
    }
    if (item.subject_type === 'channel' && item.subject_id) {
      void openChannel(item.subject_id)
      return
    }
    if (item.subject_type === 'issue' && item.subject_id) {
      navigation.navigate('IssueDetail', { issueId: item.subject_id })
      return
    }
    if (
      (item.subject_type === 'file' ||
        item.subject_type === 'document' ||
        item.subject_type === 'spreadsheet' ||
        item.subject_type === 'design') &&
      item.subject_id
    ) {
      navigation.navigate('DriveFile', { fileId: item.subject_id })
      return
    }
    if (item.subject_type === 'conversation' && item.subject_id) {
      navigation.navigate('Mailbox', { conversationId: item.subject_id })
    }
  }

  const toggleRead = async (item: InboxItem) => {
    const nextState: InboxState = item.state === 'unread' ? 'read' : 'unread'
    setItems((prev) => prev.map((x) => (x.id === item.id ? { ...x, state: nextState } : x)))
    try {
      if (nextState === 'read') await notificationApi.markRead(item.id)
      else await notificationApi.markUnread(item.id)
      void refreshUnread()
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not update item'))
    }
  }

  const toggleDone = async (item: InboxItem) => {
    const nextState: InboxState = item.state === 'done' ? 'read' : 'done'
    setItems((prev) => prev.map((x) => (x.id === item.id ? { ...x, state: nextState } : x)))
    try {
      if (nextState === 'done') await notificationApi.markDone(item.id)
      else await notificationApi.markUndone(item.id)
      void refreshUnread()
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not update item'))
    }
  }

  const toggleEvents = async (item: InboxItem) => {
    if (expandedId === item.id) {
      setExpandedId(null)
      return
    }
    setExpandedId(item.id)
    if (!eventsByItem[item.id]) {
      try {
        const res = await notificationApi.inboxEvents(item.id, undefined, 20)
        setEventsByItem((prev) => ({ ...prev, [item.id]: res.data ?? [] }))
      } catch {
        /* ignore */
      }
    }
  }

  const markAll = async () => {
    try {
      await notificationApi.markAllRead({ workspace_id: activeWorkspace?.id })
      setItems((prev) =>
        prev.map((x) => (x.state === 'unread' ? { ...x, state: 'read', unread_count: 0 } : x)),
      )
      useUiStore.getState().setUnread(0)
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to mark all read'))
    }
  }

  const cycleEmailPref = async (kind: string) => {
    if (!activeWorkspace?.id) return
    const existing = prefs.find((p) => p.kind === kind) ?? {
      kind,
      in_app: true,
      push: true,
      email: 'digest' as const,
    }
    const nextEmail: NotificationPrefRow['email'] =
      existing.email === 'digest' ? 'immediate' : existing.email === 'immediate' ? 'never' : 'digest'
    const nextPrefs = [
      ...prefs.filter((p) => p.kind !== kind),
      { ...existing, email: nextEmail },
    ]
    setPrefs(nextPrefs)
    try {
      const res = await notificationApi.replacePrefs(activeWorkspace.id, nextPrefs)
      setPrefs(res.data?.prefs ?? nextPrefs)
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not save notification preferences'))
    }
  }

  const renderItem = ({ item }: { item: InboxItem }) => {
    const legacyType = kindToLegacyType(item.top_kind)
    const isUnread = item.state === 'unread'
    const events = eventsByItem[item.id] ?? []
    return (
      <View
        style={{
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
          backgroundColor: isUnread ? theme.surface : 'transparent',
        }}
      >
        <Pressable
          onPress={() => void handleTap(item)}
          accessibilityRole="button"
          accessibilityLabel={`${LABEL_BY_TYPE[legacyType] ?? item.top_kind}${isUnread ? ', unread' : ''}: ${item.title}. ${item.preview}`}
          accessibilityHint="Opens the related item"
          style={{
            flexDirection: 'row',
            gap: 12,
            paddingHorizontal: gutter,
            paddingVertical: tier === 'compact' ? 14 : 10,
            minHeight: minTouch,
          }}
        >
          <View
            style={{
              width: 36,
              height: 36,
              borderRadius: 18,
              backgroundColor: theme.surfaceAlt,
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.body, fontSize: 16 }}>{ICON_BY_TYPE[legacyType] ?? '•'}</Text>
          </View>
          <View style={{ flex: 1 }}>
            <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8 }}>
              <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600', flex: 1 }} numberOfLines={1}>
                {item.title}
              </Text>
              {tier !== 'compact' ? (
                <Text style={{ color: theme.textFaint, fontSize: 11, fontWeight: '700', letterSpacing: 0.5 }}>
                  {(LABEL_BY_TYPE[legacyType] ?? item.top_kind).toUpperCase()}
                </Text>
              ) : null}
              <Text style={{ color: theme.textMuted, fontSize: 11, minWidth: 62, textAlign: 'right' }}>
                {relativeTime(item.last_at)}
              </Text>
            </View>
            {!!item.preview && (
              <Text style={{ color: theme.textMuted, fontSize: 13, lineHeight: 19, marginTop: 2 }} numberOfLines={2}>
                {item.preview}
              </Text>
            )}
            <View style={{ flexDirection: 'row', gap: 12, marginTop: 6 }}>
              <Pressable onPress={() => void toggleRead(item)} hitSlop={6} accessibilityRole="button">
                <Text style={{ color: theme.accent, fontSize: 12 }}>
                  {isUnread ? 'Mark read' : 'Mark unread'}
                </Text>
              </Pressable>
              <Pressable onPress={() => void toggleDone(item)} hitSlop={6} accessibilityRole="button">
                <Text style={{ color: theme.textMuted, fontSize: 12 }}>
                  {item.state === 'done' ? 'Move to inbox' : 'Done'}
                </Text>
              </Pressable>
              {item.event_count > 1 && (
                <Pressable onPress={() => void toggleEvents(item)} hitSlop={6} accessibilityRole="button">
                  <Text style={{ color: theme.textFaint, fontSize: 12 }}>
                    {expandedId === item.id ? 'Hide activity' : `${item.event_count} events`}
                  </Text>
                </Pressable>
              )}
            </View>
          </View>
          {isUnread && (
            <View
              style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: theme.primary, alignSelf: 'center' }}
            />
          )}
        </Pressable>
        {expandedId === item.id && events.length > 0 && (
          <View style={{ paddingHorizontal: gutter + 48, paddingBottom: 10, gap: 4 }}>
            {events.map((ev) => (
              <Text key={ev.id} style={{ color: theme.textFaint, fontSize: 12 }}>
                • {ev.body || ev.title} ({relativeTime(ev.created_at)})
              </Text>
            ))}
          </View>
        )}
      </View>
    )
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title="Inbox"
        onBack={() => navigation.goBack()}
        maxWidth={CONTENT_MAX_WIDTH}
        right={
          <View style={{ flexDirection: 'row', gap: 8 }}>
            <Pressable
              onPress={() => setShowPrefs((v) => !v)}
              accessibilityRole="button"
              accessibilityLabel="Notification preferences"
              hitSlop={8}
              style={{ minHeight: minTouch, justifyContent: 'center', paddingHorizontal: 8 }}
            >
              <Text style={{ color: theme.textMuted, fontSize: 13 }}>Prefs</Text>
            </Pressable>
            <Pressable
              onPress={markAll}
              accessibilityRole="button"
              accessibilityLabel="Mark all notifications read"
              hitSlop={8}
              style={{ minHeight: minTouch, justifyContent: 'center', paddingHorizontal: 8 }}
            >
              <Text style={{ color: theme.accent, fontSize: 13 }}>Mark all read</Text>
            </Pressable>
          </View>
        }
      />

      <View
        style={[
          contentColumn(),
          { flexDirection: 'row', gap: 8, paddingHorizontal: gutter, paddingVertical: 8 },
        ]}
      >
        <Chip
          label="All"
          selected={stateFilter === undefined}
          onPress={() => setStateFilter(undefined)}
        />
        <Chip
          label="Unread"
          selected={stateFilter === 'unread'}
          onPress={() => setStateFilter('unread')}
        />
        <Chip
          label="Done"
          selected={stateFilter === 'done'}
          onPress={() => setStateFilter('done')}
        />
      </View>

      {showPrefs && activeWorkspace && (
        <View
          style={[
            contentColumn(),
            {
              paddingHorizontal: gutter,
              paddingVertical: 10,
              backgroundColor: theme.surface,
              borderBottomWidth: 1,
              borderBottomColor: theme.border,
              gap: 6,
            },
          ]}
        >
          <Text style={{ color: theme.text, fontSize: 13, fontWeight: '700' }}>
            Email delivery preferences (tap to cycle)
          </Text>
          <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 6 }}>
            {DEFAULT_PREF_KINDS.map((kind) => {
              const row = prefs.find((p) => p.kind === kind)
              const email = row?.email ?? 'digest'
              return (
                <Chip
                  key={kind}
                  label={`${kind}: ${email}`}
                  selected={email !== 'never'}
                  onPress={() => void cycleEmailPref(kind)}
                />
              )
            })}
          </View>
        </View>
      )}

      {loading ? (
        <LoadingState label="Loading inbox" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => load('initial')} />
      ) : (
        <FlatList
          data={items}
          keyExtractor={(item) => item.id}
          renderItem={renderItem}
          contentContainerStyle={contentColumn()}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => {
                void load('refresh')
                void refreshUnread()
              }}
              tintColor={theme.accent}
            />
          }
          onEndReachedThreshold={0.4}
          onEndReached={() => {
            if (hasMore && !loadingMore) void load('next')
          }}
          ListFooterComponent={
            <ListFooter loading={loadingMore} hasMore={hasMore} onLoadMore={() => load('next')} />
          }
          ListEmptyComponent={<EmptyState title="You're all caught up" />}
        />
      )}
    </SafeAreaView>
  )
}
