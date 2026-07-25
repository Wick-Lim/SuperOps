import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { View, Text, SectionList, Pressable, SafeAreaView, RefreshControl } from 'react-native'
import { channelApi } from '../api/channels'
import { workspaceApi } from '../api/workspaces'
import { notificationApi } from '../api/notifications'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { useUiStore } from '../stores/uiStore'
import { useUserStore, displayName } from '../stores/userStore'
import { wsManager } from '../lib/websocket'
import { errorMessage } from '../api/client'
import { theme, presenceColor } from '../lib/theme'
import type { Channel, PresenceStatus } from '../lib/types'
import ChannelView from '../components/channel/ChannelView'
import { ErrorState, LoadingState, MIN_TOUCH, touchSlop } from './internal/ui'

const EMPTY_IDS: string[] = []

function isDM(ch: Channel): boolean {
  return ch.type === 'dm' || ch.type === 'group_dm'
}

export default function WorkspaceScreen({ navigation }: { navigation: any; route: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channels = useChannelStore((s) => s.channels)
  const activeChannel = useChannelStore((s) => s.activeChannel)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const user = useAuthStore((s) => s.user)
  const unread = useUiStore((s) => s.unreadNotifications)
  const connection = useUiStore((s) => s.connection)
  const presence = useUiStore((s) => s.presence)
  const users = useUserStore((s) => s.users)

  const workspaceId = workspace?.id

  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)
  /** channelId -> participant user ids, for DMs (whose `name` is null). */
  const [dmMembers, setDmMembers] = useState<Record<string, string[]>>({})
  const dmFetched = useRef<Set<string>>(new Set())

  const loadChannels = useCallback(
    async (mode: 'initial' | 'refresh') => {
      if (!workspaceId) return
      if (mode === 'initial') setLoading(true)
      else setRefreshing(true)
      setError(null)
      try {
        const res = await channelApi.list(workspaceId)
        useChannelStore.getState().setChannels(res.data ?? [])
      } catch (err) {
        // A failed fetch used to be swallowed, so a broken sidebar and an empty
        // workspace looked identical ("No channels yet.").
        setError(errorMessage(err, 'Could not load channels'))
      } finally {
        setLoading(false)
        setRefreshing(false)
      }
    },
    [workspaceId],
  )

  // Presence and the notification badge are decorative: a failure there must
  // not blank the sidebar, but it is still reported to the console.
  const loadSecondary = useCallback(async () => {
    if (!workspaceId) return
    const [presenceRes, unreadRes] = await Promise.allSettled([
      workspaceApi.presence(workspaceId),
      notificationApi.unreadCount(),
    ])
    if (presenceRes.status === 'fulfilled') {
      useUiStore.getState().setPresence(presenceRes.value.data ?? {})
    }
    if (unreadRes.status === 'fulfilled') {
      useUiStore.getState().setUnread(unreadRes.value.data?.count ?? 0)
    }
  }, [workspaceId])

  useEffect(() => {
    if (!workspaceId) {
      setLoading(false)
      return
    }
    // Rosters are cached per channel id; a workspace switch invalidates them.
    dmFetched.current = new Set()
    setDmMembers({})
    void loadChannels('initial')
    void loadSecondary()
    wsManager.connect()
    return () => wsManager.disconnect()
  }, [workspaceId, loadChannels, loadSecondary])

  // Re-run after a realtime resync (reconnect or dropped-frame gap).
  useEffect(() => wsManager.onResync(() => void loadSecondary()), [loadSecondary])

  /**
   * Keyed on the id list, not the array identity: the old effect unsubscribed
   * and resubscribed every channel whenever the array was replaced, which
   * happens on every refresh.
   */
  const channelIdKey = useMemo(() => channels.map((c) => c.id).sort().join(','), [channels])
  useEffect(() => {
    wsManager.setSubscriptions(channelIdKey ? channelIdKey.split(',') : [])
  }, [channelIdKey])

  /**
   * DMs have `name === null`, so every row rendered the literal string
   * "Direct message". Resolve the roster once per DM and render the members.
   */
  useEffect(() => {
    if (!workspaceId) return
    const pending = channels.filter((c) => isDM(c) && !c.name && !dmFetched.current.has(c.id))
    if (pending.length === 0) return
    pending.forEach((ch) => dmFetched.current.add(ch.id))
    void Promise.allSettled(
      pending.map(async (ch) => {
        const res = await channelApi.listMembers(workspaceId, ch.id)
        const ids = (res.data ?? []).map((m) => m.user_id)
        setDmMembers((prev) => ({ ...prev, [ch.id]: ids }))
        useUserStore.getState().ensureUsers(ids)
      }),
    )
  }, [channels, workspaceId])

  const dmTitle = useCallback(
    (ch: Channel): string => {
      if (ch.name) return ch.name
      const others = (dmMembers[ch.id] ?? EMPTY_IDS).filter((id) => id !== user?.id)
      if (others.length === 0) return dmMembers[ch.id] ? 'Just you' : 'Direct message'
      return others.map((id) => displayName(users, id)).join(', ')
    },
    [dmMembers, users, user?.id],
  )

  const sections = useMemo(() => {
    const regular = channels.filter(
      (c) => (c.type === 'public' || c.type === 'private') && !c.is_archived,
    )
    const dms = channels.filter(isDM)
    // Both sections always render: the DM section header carried the only entry
    // point to NewDM, so a user with zero DMs could never start one.
    return [
      { title: 'CHANNELS', data: regular },
      { title: 'DIRECT MESSAGES', data: dms },
    ]
  }, [channels])

  const selectChannel = (ch: Channel) => {
    setActiveChannel(ch)
    channelApi
      .markRead(ch.id)
      .then((res) => useChannelStore.getState().setUnreadCount(ch.id, res.data?.unread_count ?? 0))
      .catch(() => {
        /* the badge stays until the next unread.update */
      })
  }

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

  const headerBtn = (
    glyph: string,
    label: string,
    onPress: () => void,
    badge?: number,
  ) => (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={badge ? `${label}, ${badge} unread` : label}
      hitSlop={touchSlop(28)}
      style={{
        minWidth: MIN_TOUCH,
        minHeight: MIN_TOUCH,
        alignItems: 'center',
        justifyContent: 'center',
      }}
    >
      <Text style={{ fontSize: 18 }}>{glyph}</Text>
      {badge ? (
        <View
          style={{
            position: 'absolute',
            top: 4,
            right: 2,
            minWidth: 16,
            height: 16,
            borderRadius: 8,
            backgroundColor: theme.danger,
            alignItems: 'center',
            justifyContent: 'center',
            paddingHorizontal: 3,
          }}
        >
          <Text style={{ color: '#fff', fontSize: 10, fontWeight: '700' }}>
            {badge > 9 ? '9+' : badge}
          </Text>
        </View>
      ) : null}
    </Pressable>
  )

  const selfPresence: PresenceStatus =
    (user ? presence[user.id] : undefined) ?? (connection === 'connected' ? 'online' : 'offline')

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.surface }}>
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
        <View
          style={{
            width: 32,
            height: 32,
            backgroundColor: theme.primary,
            borderRadius: 8,
            alignItems: 'center',
            justifyContent: 'center',
            marginRight: 10,
          }}
        >
          <Text style={{ color: '#fff', fontWeight: 'bold', fontSize: 14 }}>
            {workspace?.name?.[0] || 'S'}
          </Text>
        </View>
        <Text
          accessibilityRole="header"
          style={{ color: theme.text, fontWeight: '600', fontSize: 16, flex: 1 }}
          numberOfLines={1}
        >
          {workspace?.name || 'SuperOps'}
        </Text>
        {headerBtn('🔍', 'Search messages', () => navigation.navigate('Search'))}
        {headerBtn('🔔', 'Notifications', () => navigation.navigate('Notifications'), unread)}
        {headerBtn('⚙️', 'Settings', () => navigation.navigate('Settings'))}
      </View>

      {loading ? (
        <LoadingState label="Loading channels" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => loadChannels('initial')} />
      ) : (
        <SectionList
          sections={sections}
          keyExtractor={(ch) => ch.id}
          stickySectionHeadersEnabled={false}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => {
                void loadChannels('refresh')
                void loadSecondary()
              }}
              tintColor={theme.accent}
            />
          }
          renderSectionHeader={({ section }) => {
            const isChannels = section.title === 'CHANNELS'
            return (
              <View
                style={{
                  flexDirection: 'row',
                  alignItems: 'center',
                  justifyContent: 'space-between',
                  paddingLeft: 16,
                  paddingRight: 8,
                  paddingTop: 16,
                }}
              >
                <Text
                  accessibilityRole="header"
                  style={{ color: theme.textMuted, fontSize: 11, fontWeight: '700', letterSpacing: 1 }}
                >
                  {section.title}
                </Text>
                <Pressable
                  onPress={() => navigation.navigate(isChannels ? 'NewChannel' : 'NewDM')}
                  accessibilityRole="button"
                  accessibilityLabel={isChannels ? 'Create or browse channels' : 'Start a direct message'}
                  hitSlop={touchSlop(24)}
                  style={{
                    minWidth: MIN_TOUCH,
                    minHeight: MIN_TOUCH,
                    alignItems: 'center',
                    justifyContent: 'center',
                  }}
                >
                  <Text style={{ color: theme.textMuted, fontSize: 20, fontWeight: '600' }}>＋</Text>
                </Pressable>
              </View>
            )
          }}
          renderSectionFooter={({ section }) =>
            section.data.length === 0 ? (
              <Text style={{ color: theme.textMuted, fontSize: 13, paddingHorizontal: 16, paddingVertical: 8 }}>
                {section.title === 'CHANNELS'
                  ? 'No channels yet — tap ＋ to create or browse one.'
                  : 'No conversations yet — tap ＋ to start one.'}
              </Text>
            ) : null
          }
          renderItem={({ item: ch }) => {
            const dm = isDM(ch)
            const title = dm ? dmTitle(ch) : ch.name || ch.slug || 'unnamed'
            const count = ch.unread_count ?? 0
            return (
              <Pressable
                onPress={() => selectChannel(ch)}
                onLongPress={() => navigation.navigate('ChannelDetail', { channelId: ch.id })}
                accessibilityRole="button"
                accessibilityLabel={
                  count > 0
                    ? `${dm ? 'Conversation with' : 'Channel'} ${title}, ${count} unread`
                    : `${dm ? 'Conversation with' : 'Channel'} ${title}`
                }
                accessibilityHint="Long press for channel details"
                style={{
                  paddingHorizontal: 16,
                  minHeight: MIN_TOUCH,
                  flexDirection: 'row',
                  alignItems: 'center',
                }}
              >
                <Text style={{ color: theme.textMuted, marginRight: 8 }}>{dm ? '💬' : '#'}</Text>
                <Text
                  style={{
                    color: count > 0 ? theme.text : theme.body,
                    fontSize: 15,
                    fontWeight: count > 0 ? '700' : '400',
                    flex: 1,
                  }}
                  numberOfLines={1}
                >
                  {title}
                </Text>
                {count > 0 ? (
                  <View
                    style={{
                      minWidth: 20,
                      height: 20,
                      borderRadius: 10,
                      paddingHorizontal: 6,
                      backgroundColor: theme.danger,
                      alignItems: 'center',
                      justifyContent: 'center',
                    }}
                  >
                    <Text style={{ color: '#fff', fontSize: 11, fontWeight: '700' }}>
                      {count > 99 ? '99+' : count}
                    </Text>
                  </View>
                ) : null}
              </Pressable>
            )
          }}
        />
      )}

      <Pressable
        onPress={() => navigation.navigate('Settings')}
        accessibilityRole="button"
        accessibilityLabel={`Open settings. Signed in as ${user?.full_name || user?.username || 'unknown'}, ${selfPresence}`}
        style={{
          height: 56,
          paddingHorizontal: 16,
          flexDirection: 'row',
          alignItems: 'center',
          borderTopWidth: 1,
          borderTopColor: theme.border,
        }}
      >
        <View
          style={{
            width: 32,
            height: 32,
            backgroundColor: theme.primary,
            borderRadius: 16,
            alignItems: 'center',
            justifyContent: 'center',
            marginRight: 8,
          }}
        >
          <Text style={{ color: '#fff', fontWeight: '600', fontSize: 12 }}>
            {(user?.full_name || user?.username || '?')[0]?.toUpperCase()}
          </Text>
        </View>
        <View style={{ flex: 1 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '500' }}>
            {user?.full_name || user?.username}
          </Text>
          {user?.status_text ? (
            <Text style={{ color: theme.textMuted, fontSize: 12 }} numberOfLines={1}>
              {user.status_emoji} {user.status_text}
            </Text>
          ) : null}
        </View>
        {/* Was hardcoded to green regardless of the real presence state. */}
        <View
          style={{
            width: 10,
            height: 10,
            borderRadius: 5,
            backgroundColor: presenceColor[selfPresence] ?? presenceColor.offline,
          }}
        />
      </Pressable>
    </SafeAreaView>
  )
}
