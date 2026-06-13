import React, { useEffect, useMemo, useState } from 'react'
import { View, Text, SectionList, Pressable, SafeAreaView } from 'react-native'
import { channelApi } from '../api/channels'
import { workspaceApi } from '../api/workspaces'
import { notificationApi } from '../api/notifications'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { useUiStore } from '../stores/uiStore'
import { wsManager } from '../lib/websocket'
import { theme, presenceColor } from '../lib/theme'
import type { Channel } from '../lib/types'
import ChannelView from '../components/channel/ChannelView'

export default function WorkspaceScreen({ navigation }: { navigation: any; route: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channels = useChannelStore((s) => s.channels)
  const setChannels = useChannelStore((s) => s.setChannels)
  const activeChannel = useChannelStore((s) => s.activeChannel)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const user = useAuthStore((s) => s.user)
  const unread = useUiStore((s) => s.unreadNotifications)
  const setUnread = useUiStore((s) => s.setUnread)
  const setPresence = useUiStore((s) => s.setPresence)

  const workspaceId = workspace?.id

  useEffect(() => {
    if (!workspaceId) return
    channelApi.list(workspaceId).then((res) => setChannels(res.data ?? [])).catch(() => {})
    workspaceApi.presence(workspaceId).then((res) => setPresence(res.data ?? {})).catch(() => {})
    notificationApi.unreadCount().then((res) => setUnread(res.data?.count ?? 0)).catch(() => {})
    wsManager.connect()
    return () => wsManager.disconnect()
  }, [workspaceId])

  useEffect(() => {
    channels.forEach((ch) => wsManager.subscribe(ch.id))
    return () => channels.forEach((ch) => wsManager.unsubscribe(ch.id))
  }, [channels])

  const sections = useMemo(() => {
    const regular = channels.filter((c) => (c.type === 'public' || c.type === 'private') && !c.is_archived)
    const dms = channels.filter((c) => c.type === 'dm' || c.type === 'group_dm')
    const out: Array<{ title: string; data: Channel[] }> = []
    out.push({ title: 'CHANNELS', data: regular })
    if (dms.length) out.push({ title: 'DIRECT MESSAGES', data: dms })
    return out
  }, [channels])

  const selectChannel = (ch: Channel) => {
    setActiveChannel(ch)
    channelApi.markRead(ch.id).catch(() => {})
  }

  // Channel view
  if (activeChannel) {
    return (
      <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
        <ChannelView
          channel={activeChannel}
          onBack={() => setActiveChannel(null)}
          onOpenMembers={() => navigation.navigate('Members', { channelId: activeChannel.id })}
        />
      </SafeAreaView>
    )
  }

  const headerBtn = (label: string, onPress: () => void, badge?: number) => (
    <Pressable onPress={onPress} style={{ paddingHorizontal: 6, paddingVertical: 4 }}>
      <Text style={{ fontSize: 18 }}>{label}</Text>
      {badge ? (
        <View style={{ position: 'absolute', top: 0, right: 0, minWidth: 16, height: 16, borderRadius: 8, backgroundColor: theme.danger, alignItems: 'center', justifyContent: 'center', paddingHorizontal: 3 }}>
          <Text style={{ color: '#fff', fontSize: 10, fontWeight: '700' }}>{badge > 9 ? '9+' : badge}</Text>
        </View>
      ) : null}
    </Pressable>
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.surface }}>
      {/* Header */}
      <View style={{ height: 56, paddingHorizontal: 12, flexDirection: 'row', alignItems: 'center', borderBottomWidth: 1, borderBottomColor: theme.border }}>
        <View style={{ width: 32, height: 32, backgroundColor: theme.primary, borderRadius: 8, alignItems: 'center', justifyContent: 'center', marginRight: 10 }}>
          <Text style={{ color: '#fff', fontWeight: 'bold', fontSize: 14 }}>{workspace?.name?.[0] || 'S'}</Text>
        </View>
        <Text style={{ color: theme.text, fontWeight: '600', fontSize: 16, flex: 1 }} numberOfLines={1}>{workspace?.name || 'SuperOps'}</Text>
        {headerBtn('🔍', () => navigation.navigate('Search'))}
        {headerBtn('🔔', () => navigation.navigate('Notifications'), unread)}
        {headerBtn('⚙️', () => navigation.navigate('Settings'))}
      </View>

      <SectionList
        sections={sections}
        keyExtractor={(ch) => ch.id}
        stickySectionHeadersEnabled={false}
        renderSectionHeader={({ section }) => (
          <View style={{ flexDirection: 'row', alignItems: 'center', justifyContent: 'space-between', paddingHorizontal: 16, paddingTop: 16, paddingBottom: 6 }}>
            <Text style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 1 }}>{section.title}</Text>
            <Pressable onPress={() => navigation.navigate(section.title === 'CHANNELS' ? 'NewChannel' : 'NewDM')}>
              <Text style={{ color: theme.textMuted, fontSize: 18, fontWeight: '600' }}>＋</Text>
            </Pressable>
          </View>
        )}
        renderItem={({ item: ch }) => {
          const isDM = ch.type === 'dm' || ch.type === 'group_dm'
          return (
            <Pressable onPress={() => selectChannel(ch)} style={{ paddingHorizontal: 16, paddingVertical: 10, flexDirection: 'row', alignItems: 'center' }}>
              <Text style={{ color: theme.textFaint, marginRight: 8 }}>{isDM ? '💬' : '#'}</Text>
              <Text style={{ color: theme.body, fontSize: 15 }} numberOfLines={1}>
                {ch.name || (isDM ? 'Direct message' : 'unnamed')}
              </Text>
            </Pressable>
          )
        }}
        ListEmptyComponent={
          <Text style={{ color: theme.textDim, fontSize: 14, paddingHorizontal: 16, paddingTop: 24 }}>No channels yet.</Text>
        }
      />

      {/* User footer */}
      <Pressable onPress={() => navigation.navigate('Settings')} style={{ height: 56, paddingHorizontal: 16, flexDirection: 'row', alignItems: 'center', borderTopWidth: 1, borderTopColor: theme.border }}>
        <View style={{ width: 32, height: 32, backgroundColor: theme.success, borderRadius: 16, alignItems: 'center', justifyContent: 'center', marginRight: 8 }}>
          <Text style={{ color: '#fff', fontWeight: '600', fontSize: 12 }}>{(user?.full_name || user?.username || '?')[0]?.toUpperCase()}</Text>
        </View>
        <View style={{ flex: 1 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '500' }}>{user?.full_name || user?.username}</Text>
          {user?.status_text ? <Text style={{ color: theme.textFaint, fontSize: 12 }} numberOfLines={1}>{user.status_emoji} {user.status_text}</Text> : null}
        </View>
        <View style={{ width: 8, height: 8, borderRadius: 4, backgroundColor: presenceColor.online }} />
      </Pressable>
    </SafeAreaView>
  )
}
