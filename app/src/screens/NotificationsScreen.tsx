import React, { useCallback, useEffect, useState } from 'react'
import { View, Text, Pressable, FlatList, SafeAreaView, Alert, RefreshControl } from 'react-native'
import { theme } from '../lib/theme'
import { notificationApi } from '../api/notifications'
import { channelApi } from '../api/channels'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUiStore } from '../stores/uiStore'
import { errorMessage } from '../api/client'
import type { AppNotification, NotificationType } from '../lib/types'
import { useResponsive } from '../lib/responsive'
import {
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

function parseChannelId(data: string): string | null {
  try {
    const parsed = JSON.parse(data)
    return typeof parsed?.channel_id === 'string' ? parsed.channel_id : null
  } catch {
    return null
  }
}

export default function NotificationsScreen({ navigation }: { navigation: any; route: any }) {
  const channels = useChannelStore((s) => s.channels)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const { tier, gutter, minTouch } = useResponsive()

  const [items, setItems] = useState<AppNotification[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const refreshUnread = useCallback(async () => {
    try {
      const res = await notificationApi.unreadCount()
      useUiStore.getState().setUnread(res.data?.count ?? 0)
    } catch {
      /* the badge stays as it was */
    }
  }, [])

  /** `next` pages with the cursor the API has always returned and nothing read. */
  const load = useCallback(
    async (mode: 'initial' | 'refresh' | 'next') => {
      if (mode === 'initial') setLoading(true)
      if (mode === 'refresh') setRefreshing(true)
      if (mode === 'next') setLoadingMore(true)
      if (mode !== 'next') setError(null)
      try {
        const pageCursor = mode === 'next' ? cursor : undefined
        const res = await notificationApi.list(pageCursor, PAGE_SIZE)
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
    [cursor],
  )

  useEffect(() => {
    void load('initial')
    void refreshUnread()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [])

  const openChannel = async (channelId: string) => {
    const ch = channels.find((c) => c.id === channelId)
    if (ch) {
      useChannelStore.getState().setActiveChannel(ch)
      navigation.navigate('Workspace', {})
      return
    }
    if (!activeWorkspace) return
    // A channel_invite notification arrives before the sidebar knows about the
    // channel; fetch it rather than silently doing nothing.
    try {
      const res = await channelApi.get(activeWorkspace.id, channelId)
      useChannelStore.getState().addChannel(res.data)
      useChannelStore.getState().setActiveChannel(res.data)
      navigation.navigate('Workspace', {})
    } catch (err) {
      Alert.alert('Unavailable', errorMessage(err, 'That conversation is no longer available.'))
    }
  }

  const handleTap = async (n: AppNotification) => {
    if (!n.is_read) {
      setItems((prev) => prev.map((x) => (x.id === n.id ? { ...x, is_read: true } : x)))
      try {
        await notificationApi.markRead(n.id)
        void refreshUnread()
      } catch {
        /* optimistic update stands; the count refreshes on the next load */
      }
    }
    const channelId = parseChannelId(n.data)
    if (channelId) void openChannel(channelId)
  }

  const markAll = async () => {
    try {
      await notificationApi.markAllRead()
      setItems((prev) => prev.map((x) => ({ ...x, is_read: true })))
      useUiStore.getState().setUnread(0)
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to mark all read'))
    }
  }

  const renderItem = ({ item }: { item: AppNotification }) => (
    <Pressable
      onPress={() => handleTap(item)}
      accessibilityRole="button"
      accessibilityLabel={`${LABEL_BY_TYPE[item.type] ?? 'Notification'}${item.is_read ? '' : ', unread'}: ${item.title}. ${item.body}`}
      accessibilityHint="Opens the related conversation"
      style={{
        flexDirection: 'row',
        gap: 12,
        paddingHorizontal: gutter,
        paddingVertical: tier === 'compact' ? 14 : 10,
        minHeight: minTouch,
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
        backgroundColor: item.is_read ? 'transparent' : theme.surface,
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
        <Text style={{ color: theme.body, fontSize: 16 }}>{ICON_BY_TYPE[item.type] ?? '•'}</Text>
      </View>
      <View style={{ flex: 1 }}>
        <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600', flex: 1 }} numberOfLines={1}>
            {item.title}
          </Text>
          {/* The icon is all a phone has room to say about the type; a wider
              row can spell it out instead of leaving the space empty. */}
          {tier !== 'compact' ? (
            <Text style={{ color: theme.textFaint, fontSize: 11, fontWeight: '700', letterSpacing: 0.5 }}>
              {(LABEL_BY_TYPE[item.type] ?? 'Notification').toUpperCase()}
            </Text>
          ) : null}
          <Text style={{ color: theme.textMuted, fontSize: 11, minWidth: 62, textAlign: 'right' }}>
            {relativeTime(item.created_at)}
          </Text>
        </View>
        {!!item.body && (
          <Text style={{ color: theme.textMuted, fontSize: 13, lineHeight: 19, marginTop: 2 }} numberOfLines={2}>
            {item.body}
          </Text>
        )}
      </View>
      {!item.is_read && (
        <View
          style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: theme.primary, alignSelf: 'center' }}
        />
      )}
    </Pressable>
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title="Notifications"
        onBack={() => navigation.goBack()}
        maxWidth={CONTENT_MAX_WIDTH}
        right={
          <Pressable
            onPress={markAll}
            accessibilityRole="button"
            accessibilityLabel="Mark all notifications read"
            hitSlop={8}
            style={{ minHeight: minTouch, justifyContent: 'center', paddingHorizontal: 8 }}
          >
            <Text style={{ color: theme.accent, fontSize: 13 }}>Mark all read</Text>
          </Pressable>
        }
      />

      {loading ? (
        <LoadingState label="Loading notifications" />
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
