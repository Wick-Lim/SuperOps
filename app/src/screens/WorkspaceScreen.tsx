import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import { View, Text, SectionList, Pressable, SafeAreaView, RefreshControl } from 'react-native'
import { channelApi } from '../api/channels'
import { workspaceApi } from '../api/workspaces'
import { notificationApi } from '../api/notifications'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { useMessageStore } from '../stores/messageStore'
import { useUiStore } from '../stores/uiStore'
import { useUserStore, displayName } from '../stores/userStore'
import { wsManager } from '../lib/websocket'
import { errorMessage } from '../api/client'
import { theme, presenceColor } from '../lib/theme'
import { byTier, space, useResponsive } from '../lib/responsive'
import type { Channel, Message, PresenceStatus } from '../lib/types'
import AppShell from '../components/AppShell'
import ChannelView from '../components/channel/ChannelView'
import ThreadView from '../components/message/ThreadView'
import { useCustomEmoji } from '../components/message/customEmoji'
import { Button, ErrorState, LoadingState, touchSlop } from './internal/ui'

const EMPTY_IDS: string[] = []
const EMPTY_MESSAGES: Message[] = []
/** Hoisted: a fresh style identity on every render is a fresh list re-layout. */
const LIST_CONTENT = { paddingBottom: space.lg } as const

function isDM(ch: Channel): boolean {
  return ch.type === 'dm' || ch.type === 'group_dm'
}

type SidebarSection = { title: string; data: Channel[] }

// ---------------------------------------------------------------------------
// Sidebar
// ---------------------------------------------------------------------------

/**
 * The channel list.
 *
 * Extracted from the screen so its own view state — hover, which only exists
 * for a pointer — re-renders 220px of sidebar rather than the conversation
 * beside it. It subscribes to the stores it needs directly, which is also what
 * keeps the parent from re-rendering the conversation on a presence frame.
 */
function ChannelSidebar({
  sections,
  dmTitle,
  activeChannelId,
  loading,
  error,
  refreshing,
  onSelect,
  onRefresh,
  onRetry,
  onOpenDetail,
  navigate,
}: {
  sections: SidebarSection[]
  dmTitle: (ch: Channel) => string
  activeChannelId: string | null
  loading: boolean
  error: string | null
  refreshing: boolean
  onSelect: (ch: Channel) => void
  onRefresh: () => void
  onRetry: () => void
  onOpenDetail: (channelId: string) => void
  navigate: (route: 'Search' | 'Notifications' | 'Settings' | 'NewChannel' | 'NewDM') => void
}) {
  const { tier, minTouch } = useResponsive()
  const compact = tier === 'compact'

  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const user = useAuthStore((s) => s.user)
  const unread = useUiStore((s) => s.unreadNotifications)
  const connection = useUiStore((s) => s.connection)
  const presence = useUiStore((s) => s.presence)

  /** Pointer only: `onHoverIn` never fires for touch, so this stays null there. */
  const [hovered, setHovered] = useState<string | null>(null)

  const selfPresence: PresenceStatus =
    (user ? presence[user.id] : undefined) ?? (connection === 'connected' ? 'online' : 'offline')

  const headerBtn = (glyph: string, label: string, onPress: () => void, badge?: number) => (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={badge ? `${label}, ${badge} unread` : label}
      hitSlop={touchSlop(28)}
      style={{
        minWidth: minTouch,
        minHeight: minTouch,
        alignItems: 'center',
        justifyContent: 'center',
      }}
    >
      <Text style={{ fontSize: compact ? 18 : 16 }}>{glyph}</Text>
      {badge ? (
        <View
          style={{
            position: 'absolute',
            top: 2,
            right: 0,
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

  return (
    <View style={{ flex: 1, backgroundColor: theme.surface }}>
      {/*
        Two rows once the sidebar is a fixed column, one row on a phone.
        A phone header spans the whole screen, so a name and four icons fit
        beside each other. In a 220–260px column they do not: the icons take
        ~150px and the workspace name was being truncated to "Sup…", which
        tells the reader nothing. Giving the name its own row costs ~30px of
        vertical space and buys back the entire width for it.
      */}
      <View
        style={{
          paddingHorizontal: space.md,
          paddingVertical: space.sm,
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
        }}
      >
        <View
          style={{
            minHeight: byTier(tier, { compact: 40, medium: 36 }),
            flexDirection: 'row',
            alignItems: 'center',
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
          {/* On a phone the icons share this row — there is room. */}
          {compact ? (
            <>
              {headerBtn('🔍', 'Search messages', () => navigate('Search'))}
              {headerBtn('🔔', 'Notifications', () => navigate('Notifications'), unread)}
              {headerBtn('⚙️', 'Settings', () => navigate('Settings'))}
            </>
          ) : null}
        </View>

        {!compact ? (
          <View style={{ flexDirection: 'row', alignItems: 'center', marginTop: space.xs }}>
            {headerBtn('🔍', 'Search messages', () => navigate('Search'))}
            {headerBtn('🔔', 'Notifications', () => navigate('Notifications'), unread)}
            {/* Pull-to-refresh is a thumb gesture and a mouse cannot perform it,
                so the pointer tiers get the affordance as a button instead. */}
            {headerBtn('↻', 'Refresh channels', onRefresh)}
            {headerBtn('⚙️', 'Settings', () => navigate('Settings'))}
          </View>
        ) : null}
      </View>

      {loading ? (
        <LoadingState label="Loading channels" />
      ) : error ? (
        <ErrorState message={error} onRetry={onRetry} />
      ) : (
        <SectionList
          sections={sections}
          keyExtractor={(ch) => ch.id}
          stickySectionHeadersEnabled={false}
          contentContainerStyle={LIST_CONTENT}
          refreshControl={
            compact ? (
              <RefreshControl refreshing={refreshing} onRefresh={onRefresh} tintColor={theme.accent} />
            ) : undefined
          }
          renderSectionHeader={({ section }) => {
            const isChannels = section.title === 'CHANNELS'
            // A phone hides this: the list is the whole screen, so a per-section
            // total is noise. A persistent sidebar has the room, and the count
            // is the reason you would look at a collapsed section at all.
            const sectionUnread = compact
              ? 0
              : section.data.reduce((n, ch) => n + (ch.unread_count ?? 0), 0)
            return (
              <View
                style={{
                  flexDirection: 'row',
                  alignItems: 'center',
                  paddingLeft: space.lg,
                  paddingRight: space.sm,
                  paddingTop: byTier(tier, { compact: space.lg, medium: space.md }),
                }}
              >
                <Text
                  accessibilityRole="header"
                  style={{
                    color: theme.textMuted,
                    fontSize: 11,
                    fontWeight: '700',
                    letterSpacing: 1,
                  }}
                >
                  {section.title}
                </Text>
                {sectionUnread > 0 ? (
                  <Text
                    accessibilityLabel={`${sectionUnread} unread`}
                    style={{ color: theme.textFaint, fontSize: 11, fontWeight: '700', marginLeft: space.sm }}
                  >
                    {sectionUnread > 99 ? '99+' : sectionUnread}
                  </Text>
                ) : null}
                <View style={{ flex: 1 }} />
                <Pressable
                  onPress={() => navigate(isChannels ? 'NewChannel' : 'NewDM')}
                  accessibilityRole="button"
                  accessibilityLabel={
                    isChannels ? 'Create or browse channels' : 'Start a direct message'
                  }
                  hitSlop={touchSlop(24)}
                  style={{
                    minWidth: minTouch,
                    minHeight: minTouch,
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
              <Text
                style={{
                  color: theme.textMuted,
                  fontSize: 13,
                  paddingHorizontal: space.lg,
                  paddingVertical: space.sm,
                }}
              >
                {section.title === 'CHANNELS'
                  ? `No channels yet — ${compact ? 'tap' : 'click'} ＋ to create or browse one.`
                  : `No conversations yet — ${compact ? 'tap' : 'click'} ＋ to start one.`}
              </Text>
            ) : null
          }
          renderItem={({ item: ch }) => {
            const dm = isDM(ch)
            const title = dm ? dmTitle(ch) : ch.name || ch.slug || 'unnamed'
            const count = ch.unread_count ?? 0
            // The list used to disappear behind the conversation, so there was
            // nothing for a selected state to say. Beside the conversation it is
            // the only thing tying the two panes together.
            const selected = ch.id === activeChannelId
            const background = selected
              ? theme.primary
              : hovered === ch.id
                ? theme.surfaceAlt
                : 'transparent'
            return (
              <Pressable
                onPress={() => onSelect(ch)}
                onLongPress={() => onOpenDetail(ch.id)}
                onHoverIn={() => setHovered(ch.id)}
                onHoverOut={() => setHovered((id) => (id === ch.id ? null : id))}
                accessibilityRole="button"
                accessibilityState={{ selected }}
                accessibilityLabel={
                  count > 0
                    ? `${dm ? 'Conversation with' : 'Channel'} ${title}, ${count} unread`
                    : `${dm ? 'Conversation with' : 'Channel'} ${title}`
                }
                accessibilityHint="Long press for channel details"
                style={{
                  paddingHorizontal: compact ? space.lg : space.md,
                  marginHorizontal: compact ? 0 : space.sm,
                  borderRadius: compact ? 0 : 6,
                  backgroundColor: background,
                  minHeight: minTouch,
                  flexDirection: 'row',
                  alignItems: 'center',
                }}
              >
                <Text style={{ color: selected ? theme.primaryText : theme.textMuted, marginRight: space.sm }}>
                  {dm ? '💬' : '#'}
                </Text>
                <Text
                  style={{
                    color: selected ? theme.primaryText : count > 0 ? theme.text : theme.body,
                    fontSize: 15,
                    fontWeight: count > 0 || selected ? '700' : '400',
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
                      backgroundColor: selected ? theme.primaryText : theme.danger,
                      alignItems: 'center',
                      justifyContent: 'center',
                    }}
                  >
                    <Text
                      style={{
                        color: selected ? theme.primary : '#fff',
                        fontSize: 11,
                        fontWeight: '700',
                      }}
                    >
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
        onPress={() => navigate('Settings')}
        accessibilityRole="button"
        accessibilityLabel={`Open settings. Signed in as ${user?.full_name || user?.username || 'unknown'}, ${selfPresence}`}
        style={{
          minHeight: byTier(tier, { compact: 56, medium: 48 }),
          paddingHorizontal: space.lg,
          paddingVertical: space.sm,
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
            marginRight: space.sm,
          }}
        >
          <Text style={{ color: '#fff', fontWeight: '600', fontSize: 12 }}>
            {(user?.full_name || user?.username || '?')[0]?.toUpperCase()}
          </Text>
        </View>
        <View style={{ flex: 1 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '500' }} numberOfLines={1}>
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
    </View>
  )
}

// ---------------------------------------------------------------------------
// Empty conversation pane
// ---------------------------------------------------------------------------

/**
 * What the conversation pane shows before anything is selected.
 *
 * This state does not exist on a phone — the sidebar IS the screen until you
 * pick something — so the pane would otherwise be a large, silent void the first
 * time the app is opened on a monitor.
 */
function EmptyConversation({
  workspaceName,
  hasChannels,
  onBrowse,
  onNewDM,
}: {
  workspaceName: string
  hasChannels: boolean
  onBrowse: () => void
  onNewDM: () => void
}) {
  return (
    <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center', padding: space.xxxl }}>
      {/* Short, centred copy: a reading measure this narrow keeps it a block
          rather than a line running the width of a 1600px pane. */}
      <View style={{ maxWidth: 420, alignItems: 'center' }}>
        <Text
          accessibilityRole="header"
          style={{ color: theme.text, fontSize: 20, fontWeight: '700', textAlign: 'center' }}
        >
          {workspaceName}
        </Text>
        <Text
          style={{
            color: theme.textMuted,
            fontSize: 14,
            lineHeight: 21,
            textAlign: 'center',
            marginTop: space.sm,
          }}
        >
          {hasChannels
            ? 'Choose a channel or a direct message on the left to start reading. It stays open while you browse the list.'
            : 'Nothing here yet. Create your first channel, or start a direct message with someone on your team.'}
        </Text>
        <View style={{ flexDirection: 'row', gap: space.md, marginTop: space.xl }}>
          <View style={{ minWidth: 170 }}>
            <Button label="Browse channels" onPress={onBrowse} />
          </View>
          <View style={{ minWidth: 170 }}>
            <Button label="New message" onPress={onNewDM} variant="ghost" />
          </View>
        </View>
      </View>
    </View>
  )
}

// ---------------------------------------------------------------------------
// Screen
// ---------------------------------------------------------------------------

export default function WorkspaceScreen({ navigation }: { navigation: any; route: any }) {
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channels = useChannelStore((s) => s.channels)
  const activeChannel = useChannelStore((s) => s.activeChannel)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const user = useAuthStore((s) => s.user)
  const users = useUserStore((s) => s.users)
  const activeThread = useUiStore((s) => s.activeThread)
  const customEmoji = useCustomEmoji()

  const { twoPane } = useResponsive()
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

  const sections = useMemo<SidebarSection[]>(() => {
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

  const selectChannel = useCallback((ch: Channel) => {
    // Switching channels with a thread open would otherwise leave the thread
    // pane showing a message from the channel you just left.
    useUiStore.getState().closeThread()
    useChannelStore.getState().setActiveChannel(ch)
    channelApi
      .markRead(ch.id)
      .then((res) => useChannelStore.getState().setUnreadCount(ch.id, res.data?.unread_count ?? 0))
      .catch(() => {
        /* the badge stays until the next unread.update */
      })
  }, [])

  const navigate = useCallback(
    (route: 'Search' | 'Notifications' | 'Settings' | 'NewChannel' | 'NewDM') =>
      navigation.navigate(route),
    [navigation],
  )

  const openDetail = useCallback(
    (channelId: string) => navigation.navigate('ChannelDetail', { channelId }),
    [navigation],
  )

  const refresh = useCallback(() => {
    void loadChannels('refresh')
    void loadSecondary()
  }, [loadChannels, loadSecondary])

  const closeThread = useCallback(() => useUiStore.getState().closeThread(), [])

  /**
   * The thread is rendered here, not inside ChannelView, because only the shell
   * knows whether it is a third column, an overlay or a full screen — and it has
   * to outlive a re-layout across a breakpoint either way.
   */
  const threadChannel = activeThread
    ? channels.find((c) => c.id === activeThread.channelId) ?? null
    : null

  const onReplied = useCallback((rootId: string) => {
    const thread = useUiStore.getState().activeThread
    if (!thread) return
    const store = useMessageStore.getState()
    const current = (store.messages[thread.channelId] ?? EMPTY_MESSAGES).find((m) => m.id === rootId)
    if (current) {
      store.updateMessage(thread.channelId, { ...current, reply_count: current.reply_count + 1 })
    }
  }, [])

  const conversation = activeChannel ? (
    <ChannelView
      channel={activeChannel}
      // Hidden once the sidebar is permanently on screen: there is nothing to go
      // back to. Still wired, so the compact back button behaves as it always did.
      showBack={!twoPane}
      onBack={() => setActiveChannel(null)}
      onOpenMembers={() => navigation.navigate('Members', { channelId: activeChannel.id })}
    />
  ) : null

  const thread = activeThread ? (
    <ThreadView
      visible
      root={activeThread.parent}
      channelId={activeThread.channelId}
      channelName={threadChannel ? dmTitle(threadChannel) : undefined}
      customEmoji={customEmoji}
      onClose={closeThread}
      onReplied={onReplied}
    />
  ) : null

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <AppShell
        sidebar={
          <ChannelSidebar
            sections={sections}
            dmTitle={dmTitle}
            activeChannelId={activeChannel?.id ?? null}
            loading={loading}
            error={error}
            refreshing={refreshing}
            onSelect={selectChannel}
            onRefresh={refresh}
            onRetry={() => loadChannels('initial')}
            onOpenDetail={openDetail}
            navigate={navigate}
          />
        }
        conversation={conversation}
        conversationEmpty={
          <EmptyConversation
            workspaceName={workspace?.name || 'SuperOps'}
            hasChannels={channels.length > 0}
            onBrowse={() => navigate('NewChannel')}
            onNewDM={() => navigate('NewDM')}
          />
        }
        thread={thread}
        onDismissThread={closeThread}
      />
    </SafeAreaView>
  )
}
