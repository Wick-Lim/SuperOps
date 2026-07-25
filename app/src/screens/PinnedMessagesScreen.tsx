import React, { useCallback, useEffect, useState } from 'react'
import { SafeAreaView, FlatList, Alert, RefreshControl } from 'react-native'
import { theme } from '../lib/theme'
import { messageApi } from '../api/messages'
import { useChannelStore } from '../stores/channelStore'
import { useUserStore } from '../stores/userStore'
import { errorMessage } from '../api/client'
import type { Message } from '../lib/types'
import MessageRow from './internal/MessageRow'
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

/**
 * `GET /channels/{id}/pins` had no UI at all: pinning was a write-only feature.
 */
export default function PinnedMessagesScreen({ navigation, route }: { navigation: any; route: any }) {
  const channelId: string = route.params?.channelId
  const channel = useChannelStore((s) => s.channels.find((c) => c.id === channelId) ?? null)
  const users = useUserStore((s) => s.users)

  const [items, setItems] = useState<Message[]>([])
  const [cursor, setCursor] = useState<string | undefined>(undefined)
  const [hasMore, setHasMore] = useState(false)
  const [loading, setLoading] = useState(true)
  const [loadingMore, setLoadingMore] = useState(false)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  const [busyId, setBusyId] = useState<string | null>(null)

  const load = useCallback(
    async (mode: 'initial' | 'refresh' | 'next', pageCursor?: string) => {
      if (!channelId) {
        setError('Missing channel context')
        setLoading(false)
        return
      }
      if (mode === 'initial') setLoading(true)
      if (mode === 'refresh') setRefreshing(true)
      if (mode === 'next') setLoadingMore(true)
      if (mode !== 'next') setError(null)
      try {
        const res = await messageApi.listPins(channelId, mode === 'next' ? pageCursor : undefined, PAGE_SIZE)
        const page = res.data ?? []
        setItems((prev) => (mode === 'next' ? [...prev, ...page] : page))
        setCursor(res.meta?.cursor)
        setHasMore(!!res.meta?.has_more && !!res.meta?.cursor)
        useUserStore.getState().ensureUsers(page.map((m) => m.user_id))
      } catch (err) {
        if (mode === 'next') Alert.alert('Error', errorMessage(err, 'Could not load more'))
        else setError(errorMessage(err, 'Failed to load pinned messages'))
      } finally {
        setLoading(false)
        setRefreshing(false)
        setLoadingMore(false)
      }
    },
    [channelId],
  )

  useEffect(() => {
    void load('initial')
  }, [load])

  const unpin = (m: Message) => {
    Alert.alert('Unpin message', 'Remove this message from the pinned list?', [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Unpin',
        style: 'destructive',
        onPress: async () => {
          setBusyId(m.id)
          try {
            await messageApi.unpin(channelId, m.id)
            setItems((prev) => prev.filter((x) => x.id !== m.id))
          } catch (err) {
            Alert.alert('Error', errorMessage(err, 'Could not unpin that message'))
          } finally {
            setBusyId(null)
          }
        },
      },
    ])
  }

  const openChannel = () => {
    if (channel) {
      useChannelStore.getState().setActiveChannel(channel)
      navigation.navigate('Workspace', {})
    }
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title="Pinned"
        subtitle={channel?.name ? `#${channel.name}` : undefined}
        onBack={() => navigation.goBack()}
        maxWidth={CONTENT_MAX_WIDTH}
      />
      {loading ? (
        <LoadingState label="Loading pinned messages" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => load('initial')} />
      ) : (
        <FlatList
          data={items}
          keyExtractor={(m) => m.id}
          // Message text is prose: it stops at the reading measure and centres
          // instead of running the width of a monitor.
          contentContainerStyle={contentColumn()}
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
              title="Nothing pinned yet"
              body="Long-press a message in the channel and choose Pin to keep it here."
            />
          }
          renderItem={({ item }) => (
            <MessageRow
              message={item}
              users={users}
              meta={item.pinned_at ? `pinned ${new Date(item.pinned_at).toLocaleDateString()}` : undefined}
              onPress={openChannel}
              actionLabel="Unpin"
              actionDestructive
              actionBusy={busyId === item.id}
              onAction={() => unpin(item)}
            />
          )}
        />
      )}
    </SafeAreaView>
  )
}
