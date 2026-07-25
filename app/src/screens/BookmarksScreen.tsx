import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, FlatList, Alert, RefreshControl } from 'react-native'
import { theme } from '../lib/theme'
import { messageApi } from '../api/messages'
import { channelApi } from '../api/channels'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUserStore } from '../stores/userStore'
import { errorMessage } from '../api/client'
import type { Bookmark } from '../lib/types'
import MessageRow from './internal/MessageRow'
import { EmptyState, ErrorState, ListFooter, LoadingState, ScreenHeader } from './internal/ui'

const PAGE_SIZE = 30

/**
 * `GET /bookmarks` + `DELETE /messages/{id}/bookmark` had no UI, so a bookmark
 * could be created and never seen again.
 */
export default function BookmarksScreen({ navigation }: { navigation: any; route: any }) {
  const channels = useChannelStore((s) => s.channels)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const users = useUserStore((s) => s.users)

  const [items, setItems] = useState<Bookmark[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyId, setBusyId] = useState<string | null>(null)

  const load = useCallback(
    async (mode: 'initial' | 'refresh' | 'next', pageCursor?: string) => {
      if (mode === 'initial') setLoading(true)
      if (mode === 'refresh') setRefreshing(true)
      if (mode === 'next') setLoadingMore(true)
      if (mode !== 'next') setError(null)
      try {
        const res = await messageApi.listBookmarks(mode === 'next' ? pageCursor : undefined, PAGE_SIZE)
        const page = res.data ?? []
        setItems((prev) => (mode === 'next' ? [...prev, ...page] : page))
        setCursor(res.meta?.cursor)
        setHasMore(!!res.meta?.has_more && !!res.meta?.cursor)
        useUserStore.getState().ensureUsers(page.map((b) => b.message.user_id))
      } catch (err) {
        if (mode === 'next') Alert.alert('Error', errorMessage(err, 'Could not load more'))
        else setError(errorMessage(err, 'Failed to load bookmarks'))
      } finally {
        setLoading(false)
        setRefreshing(false)
        setLoadingMore(false)
      }
    },
    [],
  )

  useEffect(() => {
    void load('initial')
  }, [load])

  const remove = async (b: Bookmark) => {
    setBusyId(b.message.id)
    try {
      await messageApi.unbookmark(b.message.id)
      setItems((prev) => prev.filter((x) => x.message.id !== b.message.id))
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not remove that bookmark'))
    } finally {
      setBusyId(null)
    }
  }

  const channelLabel = (channelId: string) => {
    const ch = channels.find((c) => c.id === channelId)
    if (!ch) return 'another channel'
    return ch.name ? `#${ch.name}` : 'direct message'
  }

  const open = async (channelId: string) => {
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
      Alert.alert('Unavailable', errorMessage(err, 'That channel is no longer available.'))
    }
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader title="Bookmarks" onBack={() => navigation.goBack()} />
      {loading ? (
        <LoadingState label="Loading bookmarks" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => load('initial')} />
      ) : (
        <FlatList
          data={items}
          keyExtractor={(b) => b.message.id}
          refreshControl={
            <RefreshControl refreshing={refreshing} onRefresh={() => load('refresh')} tintColor={theme.accent} />
          }
          onEndReachedThreshold={0.4}
          onEndReached={() => {
            if (hasMore && !loadingMore) void load('next', cursor)
          }}
          ListFooterComponent={
            <ListFooter loading={loadingMore} hasMore={hasMore} onLoadMore={() => load('next', cursor)} />
          }
          ListEmptyComponent={
            <EmptyState
              title="No bookmarks yet"
              body="Long-press a message and choose Bookmark to save it for later."
            />
          }
          renderItem={({ item }) => (
            <MessageRow
              message={item.message}
              users={users}
              meta={`${channelLabel(item.message.channel_id)} · saved ${new Date(item.bookmarked_at).toLocaleDateString()}`}
              onPress={() => open(item.message.channel_id)}
              actionLabel="Remove"
              actionDestructive
              actionBusy={busyId === item.message.id}
              onAction={() => remove(item)}
            />
          )}
        />
      )}
    </SafeAreaView>
  )
}
