import React, { useEffect, useState } from 'react'
import { View, Text, Pressable, FlatList, SafeAreaView, ActivityIndicator, Alert } from 'react-native'
import { theme } from '../lib/theme'
import { notificationApi } from '../api/notifications'
import { useChannelStore } from '../stores/channelStore'
import { useUiStore } from '../stores/uiStore'
import type { AppNotification, NotificationType } from '../lib/types'

const ICON_BY_TYPE: Record<NotificationType, string> = {
  mention: '@',
  dm: '💬',
  thread_reply: '↩',
  channel_invite: '#',
  system: '★',
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
    return parsed?.channel_id ?? null
  } catch {
    return null
  }
}

export default function NotificationsScreen({ navigation }: { navigation: any; route: any }) {
  const channels = useChannelStore((s) => s.channels)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const setUnread = useUiStore((s) => s.setUnread)

  const [items, setItems] = useState<AppNotification[]>([])
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = async () => {
    setLoading(true)
    setError(null)
    try {
      const res = await notificationApi.list()
      setItems(res.data ?? [])
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to load notifications')
      setItems([])
    } finally {
      setLoading(false)
    }
  }

  const refreshUnread = async () => {
    try {
      const res = await notificationApi.unreadCount()
      setUnread(res.data?.count ?? 0)
    } catch {
      // ignore
    }
  }

  useEffect(() => {
    load()
    refreshUnread()
  }, [])

  const handleTap = async (n: AppNotification) => {
    if (!n.is_read) {
      setItems((prev) => prev.map((x) => (x.id === n.id ? { ...x, is_read: true } : x)))
      try {
        await notificationApi.markRead(n.id)
        refreshUnread()
      } catch {
        // ignore — optimistic update already applied
      }
    }
    const channelId = parseChannelId(n.data)
    if (channelId) {
      const ch = channels.find((c) => c.id === channelId)
      if (ch) {
        setActiveChannel(ch)
        navigation.navigate('Workspace', {})
      }
    }
  }

  const markAll = async () => {
    try {
      await notificationApi.markAllRead()
      setItems((prev) => prev.map((x) => ({ ...x, is_read: true })))
      setUnread(0)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to mark all read')
    }
  }

  const renderItem = ({ item }: { item: AppNotification }) => (
    <Pressable
      onPress={() => handleTap(item)}
      style={{
        flexDirection: 'row',
        gap: 12,
        paddingHorizontal: 16,
        paddingVertical: 14,
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
        backgroundColor: item.is_read ? 'transparent' : theme.surface,
      }}
    >
      <View style={{ width: 36, height: 36, borderRadius: 18, backgroundColor: theme.surfaceAlt, alignItems: 'center', justifyContent: 'center' }}>
        <Text style={{ color: theme.body, fontSize: 16 }}>{ICON_BY_TYPE[item.type] ?? '•'}</Text>
      </View>
      <View style={{ flex: 1 }}>
        <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600', flex: 1 }} numberOfLines={1}>{item.title}</Text>
          <Text style={{ color: theme.textFaint, fontSize: 11 }}>{relativeTime(item.created_at)}</Text>
        </View>
        {!!item.body && (
          <Text style={{ color: theme.textMuted, fontSize: 13, lineHeight: 19, marginTop: 2 }} numberOfLines={2}>{item.body}</Text>
        )}
      </View>
      {!item.is_read && (
        <View style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: theme.primary, alignSelf: 'center' }} />
      )}
    </Pressable>
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      {/* Header */}
      <View style={{ height: 56, paddingHorizontal: 12, flexDirection: 'row', alignItems: 'center', borderBottomWidth: 1, borderBottomColor: theme.border }}>
        <Pressable onPress={() => navigation.goBack()} hitSlop={8} style={{ paddingHorizontal: 8, paddingVertical: 6 }}>
          <Text style={{ color: theme.accent, fontSize: 15 }}>Back</Text>
        </Pressable>
        <Text style={{ color: theme.text, fontSize: 16, fontWeight: '600', flex: 1, marginLeft: 4 }}>Notifications</Text>
        <Pressable onPress={markAll} hitSlop={8} style={{ paddingHorizontal: 8, paddingVertical: 6 }}>
          <Text style={{ color: theme.accent, fontSize: 13 }}>Mark all read</Text>
        </Pressable>
      </View>

      {loading ? (
        <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center' }}>
          <ActivityIndicator color={theme.accent} />
        </View>
      ) : error ? (
        <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center', paddingHorizontal: 32 }}>
          <Text style={{ color: theme.danger, fontSize: 15, textAlign: 'center', marginBottom: 16 }}>{error}</Text>
          <Pressable onPress={load} style={{ backgroundColor: theme.primary, borderRadius: 10, paddingHorizontal: 20, paddingVertical: 10 }}>
            <Text style={{ color: theme.primaryText, fontSize: 14, fontWeight: '600' }}>Retry</Text>
          </Pressable>
        </View>
      ) : (
        <FlatList
          data={items}
          keyExtractor={(item) => item.id}
          renderItem={renderItem}
          ListEmptyComponent={
            <View style={{ alignItems: 'center', justifyContent: 'center', paddingTop: 64, paddingHorizontal: 32 }}>
              <Text style={{ color: theme.textFaint, fontSize: 15, textAlign: 'center' }}>You're all caught up.</Text>
            </View>
          }
        />
      )}
    </SafeAreaView>
  )
}
