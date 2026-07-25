import React, { useCallback, useEffect, useMemo, useRef, useState } from 'react'
import {
  View,
  Text,
  TextInput,
  Pressable,
  SafeAreaView,
  FlatList,
  Alert,
  RefreshControl,
} from 'react-native'
import { theme, avatarColor, presenceColor } from '../lib/theme'
import type { ChannelMemberView, PresenceStatus, PublicUser } from '../lib/types'
import { channelApi } from '../api/channels'
import { workspaceApi } from '../api/workspaces'
import { userApi } from '../api/users'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { useUiStore } from '../stores/uiStore'
import { useUserStore, displayName } from '../stores/userStore'
import { errorMessage } from '../api/client'
import { useWorkspaceRole } from './internal/useWorkspaceRole'
import { useResponsive } from '../lib/responsive'
import {
  Chip,
  type Column,
  EmptyState,
  ErrorState,
  LoadingState,
  ScreenHeader,
  TABLE_MAX_WIDTH,
  TableHeader,
  cell,
  contentColumn,
} from './internal/ui'

function roleLabel(role: string): string {
  if (!role) return ''
  return role.charAt(0).toUpperCase() + role.slice(1)
}

/**
 * The roster is a table pretending to be a phone list: every row carries the
 * same four facts. Once there is width for columns, presence and role stop
 * being a second line under the name and become fields you can scan down.
 */
function memberColumns(canManage: boolean): Column[] {
  const cols: Column[] = [
    { key: 'member', label: 'Member', flex: 1 },
    { key: 'status', label: 'Status', width: 96 },
    { key: 'role', label: 'Role', width: 88 },
  ]
  if (canManage) cols.push({ key: 'actions', label: '', width: 76, align: 'right' })
  return cols
}

export default function MembersScreen({ navigation, route }: { navigation: any; route: any }) {
  const channelId: string | undefined = route.params?.channelId
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channel = useChannelStore((s) => s.channels.find((c) => c.id === channelId) ?? null)
  const me = useAuthStore((s) => s.user)
  const users = useUserStore((s) => s.users)
  // Presence lives in the ui store so live `presence.changed` frames land here
  // too; this screen used to keep its own copy and never updated.
  const presence = useUiStore((s) => s.presence)
  const { isAdmin: workspaceIsAdmin } = useWorkspaceRole()
  const { tier, gutter, minTouch } = useResponsive()
  const table = tier !== 'compact'

  const [members, setMembers] = useState<ChannelMemberView[]>([])
  const [loading, setLoading] = useState(true)
  const [refreshing, setRefreshing] = useState(false)
  const [error, setError] = useState<string | null>(null)

  const [adding, setAdding] = useState(false)
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<PublicUser[]>([])
  const [busyUserId, setBusyUserId] = useState<string | null>(null)
  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null)

  const isDM = channel?.type === 'dm' || channel?.type === 'group_dm'
  const myMembership = useMemo(() => members.find((m) => m.user_id === me?.id), [members, me?.id])
  // Mirrors backend `canAdministerChannel`: channel admin OR workspace admin.
  // DMs have a fixed roster; the backend answers 409 CANNOT_MODIFY_DM.
  const canManage = !isDM && (myMembership?.role === 'admin' || workspaceIsAdmin)
  const columns = useMemo(() => memberColumns(canManage), [canManage])

  const load = useCallback(
    async (mode: 'initial' | 'refresh' = 'initial') => {
      if (!activeWorkspace || !channelId) {
        setError('Missing channel context')
        setLoading(false)
        return
      }
      if (mode === 'initial') setLoading(true)
      else setRefreshing(true)
      setError(null)
      try {
        const res = await channelApi.listMembers(activeWorkspace.id, channelId)
        const list = res.data ?? []
        setMembers(list)
        useUserStore.getState().ensureUsers(list.map((m) => m.user_id))
      } catch (err) {
        setError(errorMessage(err, 'Failed to load members'))
      } finally {
        setLoading(false)
        setRefreshing(false)
      }
    },
    [activeWorkspace, channelId],
  )

  useEffect(() => {
    void load('initial')
  }, [load])

  useEffect(() => {
    if (!activeWorkspace) return
    workspaceApi
      .presence(activeWorkspace.id)
      .then((res) => useUiStore.getState().setPresence(res.data ?? {}))
      .catch(() => {
        /* everyone renders as offline until the next presence frame */
      })
  }, [activeWorkspace?.id])

  useEffect(() => {
    if (debounce.current) clearTimeout(debounce.current)
    const q = query.trim()
    if (!q) {
      setResults([])
      return
    }
    debounce.current = setTimeout(() => {
      userApi
        .search(q)
        .then((res) => setResults(res.data ?? []))
        .catch(() => setResults([]))
    }, 300)
    return () => {
      if (debounce.current) clearTimeout(debounce.current)
    }
  }, [query])

  const addMember = async (u: PublicUser) => {
    if (!activeWorkspace || !channelId) return
    setBusyUserId(u.id)
    try {
      const res = await channelApi.addMember(activeWorkspace.id, channelId, u.id)
      setMembers((prev) => (prev.some((m) => m.user_id === u.id) ? prev : [...prev, res.data]))
      useUserStore.getState().setUser(u)
      setQuery('')
      setResults([])
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not add that person'))
    } finally {
      setBusyUserId(null)
    }
  }

  const removeMember = (m: ChannelMemberView) => {
    const name = displayName(users, m.user_id)
    Alert.alert('Remove member', `Remove ${name} from this channel?`, [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Remove',
        style: 'destructive',
        onPress: async () => {
          if (!activeWorkspace || !channelId) return
          setBusyUserId(m.user_id)
          try {
            await channelApi.removeMember(activeWorkspace.id, channelId, m.user_id)
            setMembers((prev) => prev.filter((x) => x.user_id !== m.user_id))
          } catch (err) {
            Alert.alert('Error', errorMessage(err, 'Could not remove that member'))
          } finally {
            setBusyUserId(null)
          }
        },
      },
    ])
  }

  const memberIds = useMemo(() => new Set(members.map((m) => m.user_id)), [members])

  /** Avatar with its presence dot; smaller once rows are a table. */
  const avatar = (userId: string, name: string, dotColor: string, size: number) => (
    <View>
      <View
        style={{
          width: size,
          height: size,
          borderRadius: size / 2,
          backgroundColor: avatarColor(userId),
          alignItems: 'center',
          justifyContent: 'center',
        }}
      >
        <Text style={{ color: '#fff', fontWeight: '700', fontSize: size < 34 ? 12 : 15 }}>
          {(name || '?').charAt(0).toUpperCase()}
        </Text>
      </View>
      <View
        style={{
          position: 'absolute',
          right: -1,
          bottom: -1,
          width: 13,
          height: 13,
          borderRadius: 7,
          backgroundColor: dotColor,
          borderWidth: 2,
          borderColor: theme.bg,
        }}
      />
    </View>
  )

  const removeButton = (m: ChannelMemberView, name: string) => (
    <Pressable
      onPress={() => removeMember(m)}
      disabled={busyUserId === m.user_id}
      accessibilityRole="button"
      accessibilityLabel={`Remove ${name} from the channel`}
      hitSlop={8}
      style={{
        minHeight: minTouch,
        minWidth: minTouch,
        alignItems: 'flex-end',
        justifyContent: 'center',
        opacity: busyUserId === m.user_id ? 0.5 : 1,
      }}
    >
      <Text style={{ color: theme.danger, fontSize: 13, fontWeight: '600' }}>Remove</Text>
    </Pressable>
  )

  const roleBadge = (role: string) => (
    <View
      style={{
        paddingHorizontal: 10,
        paddingVertical: 4,
        borderRadius: 6,
        backgroundColor: theme.surfaceAlt,
      }}
    >
      <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '600' }}>{roleLabel(role)}</Text>
    </View>
  )

  const renderMember = ({ item: m }: { item: ChannelMemberView }) => {
    const name = displayName(users, m.user_id)
    const status: PresenceStatus = presence[m.user_id] ?? 'offline'
    const dotColor = presenceColor[status] ?? presenceColor.offline
    const label = `${name}, ${status}${m.role ? `, ${roleLabel(m.role)}` : ''}`

    if (table) {
      return (
        <View
          accessible
          accessibilityLabel={label}
          style={{
            flexDirection: 'row',
            alignItems: 'center',
            gap: 12,
            paddingHorizontal: gutter,
            paddingVertical: 6,
            minHeight: minTouch,
            borderBottomWidth: 1,
            borderBottomColor: theme.border,
          }}
        >
          <View style={{ ...cell(columns[0]), flexDirection: 'row', alignItems: 'center', gap: 10 }}>
            {avatar(m.user_id, name, dotColor, 28)}
            <Text style={{ color: theme.body, fontSize: 15, fontWeight: '500' }} numberOfLines={1}>
              {name}
              {m.user_id === me?.id ? ' (you)' : ''}
            </Text>
          </View>
          <View style={cell(columns[1])}>
            <Text style={{ color: theme.textMuted, fontSize: 13, textTransform: 'capitalize' }}>{status}</Text>
          </View>
          <View style={cell(columns[2])}>{m.role ? roleBadge(m.role) : null}</View>
          {canManage ? (
            <View style={cell(columns[3])}>
              {m.user_id !== me?.id ? removeButton(m, name) : null}
            </View>
          ) : null}
        </View>
      )
    }

    return (
      <View
        accessible
        accessibilityLabel={label}
        style={{
          flexDirection: 'row',
          alignItems: 'center',
          paddingHorizontal: gutter,
          paddingVertical: 12,
          minHeight: minTouch,
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
        }}
      >
        <View style={{ marginRight: 12 }}>{avatar(m.user_id, name, dotColor, 40)}</View>
        <View style={{ flex: 1 }}>
          <Text style={{ color: theme.body, fontSize: 15, fontWeight: '500' }}>
            {name}
            {m.user_id === me?.id ? ' (you)' : ''}
          </Text>
          <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 1, textTransform: 'capitalize' }}>
            {status}
          </Text>
        </View>
        {m.role ? (
          <View style={{ marginRight: canManage && m.user_id !== me?.id ? 8 : 0 }}>{roleBadge(m.role)}</View>
        ) : null}
        {canManage && m.user_id !== me?.id ? removeButton(m, name) : null}
      </View>
    )
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title={`Members${members.length ? ` (${members.length})` : ''}`}
        subtitle={channel?.name ? `#${channel.name}` : undefined}
        onBack={() => navigation.goBack()}
        maxWidth={TABLE_MAX_WIDTH}
        right={
          canManage ? (
            <Pressable
              onPress={() => setAdding((v) => !v)}
              accessibilityRole="button"
              accessibilityLabel={adding ? 'Close add member' : 'Add member'}
              accessibilityState={{ expanded: adding }}
              hitSlop={8}
              style={{ minHeight: minTouch, minWidth: minTouch, alignItems: 'center', justifyContent: 'center' }}
            >
              <Text style={{ color: theme.accent, fontSize: 14, fontWeight: '600' }}>{adding ? 'Done' : 'Add'}</Text>
            </Pressable>
          ) : undefined
        }
      />

      {adding && canManage ? (
        <View
          style={{
            ...contentColumn(TABLE_MAX_WIDTH),
            paddingHorizontal: gutter,
            paddingTop: 12,
            paddingBottom: 4,
          }}
        >
          <TextInput
            value={query}
            onChangeText={setQuery}
            placeholder="Search people to add…"
            placeholderTextColor={theme.textMuted}
            autoCapitalize="none"
            autoCorrect={false}
            accessibilityLabel="Search people to add"
            style={{
              backgroundColor: theme.surface,
              borderWidth: 1,
              borderColor: theme.borderStrong,
              borderRadius: 12,
              paddingHorizontal: 14,
              paddingVertical: table ? 9 : 12,
              minHeight: minTouch,
              color: theme.text,
              fontSize: 15,
            }}
          />
          <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 8, paddingTop: 10 }}>
            {results
              .filter((u) => !memberIds.has(u.id))
              .map((u) => (
                <Chip
                  key={u.id}
                  label={`＋ ${u.full_name || u.username}`}
                  selected={false}
                  onPress={() => addMember(u)}
                  accessibilityLabel={`Add ${u.full_name || u.username} to the channel`}
                />
              ))}
          </View>
        </View>
      ) : null}

      {loading ? (
        <LoadingState label="Loading members" />
      ) : error ? (
        <ErrorState message={error} onRetry={() => load('initial')} />
      ) : (
        <FlatList
          data={members}
          keyExtractor={(m) => m.user_id}
          renderItem={renderMember}
          contentContainerStyle={contentColumn(TABLE_MAX_WIDTH)}
          // Column labels only mean something where there are columns.
          ListHeaderComponent={table && members.length > 0 ? <TableHeader columns={columns} /> : null}
          refreshControl={
            <RefreshControl refreshing={refreshing} onRefresh={() => load('refresh')} tintColor={theme.accent} />
          }
          ListEmptyComponent={<EmptyState title="No members in this channel" />}
        />
      )}
    </SafeAreaView>
  )
}
