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
import {
  Chip,
  EmptyState,
  ErrorState,
  LoadingState,
  MIN_TOUCH,
  ScreenHeader,
} from './internal/ui'

function roleLabel(role: string): string {
  if (!role) return ''
  return role.charAt(0).toUpperCase() + role.slice(1)
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

  const renderMember = ({ item: m }: { item: ChannelMemberView }) => {
    const name = displayName(users, m.user_id)
    const status: PresenceStatus = presence[m.user_id] ?? 'offline'
    const dotColor = presenceColor[status] ?? presenceColor.offline
    return (
      <View
        accessible
        accessibilityLabel={`${name}, ${status}${m.role ? `, ${roleLabel(m.role)}` : ''}`}
        style={{
          flexDirection: 'row',
          alignItems: 'center',
          paddingHorizontal: 16,
          paddingVertical: 12,
          minHeight: MIN_TOUCH,
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
        }}
      >
        <View style={{ marginRight: 12 }}>
          <View
            style={{
              width: 40,
              height: 40,
              borderRadius: 20,
              backgroundColor: avatarColor(m.user_id),
              alignItems: 'center',
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: '#fff', fontWeight: '700', fontSize: 15 }}>
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
          <View
            style={{
              paddingHorizontal: 10,
              paddingVertical: 4,
              borderRadius: 6,
              backgroundColor: theme.surfaceAlt,
              marginRight: canManage && m.user_id !== me?.id ? 8 : 0,
            }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '600' }}>{roleLabel(m.role)}</Text>
          </View>
        ) : null}
        {canManage && m.user_id !== me?.id ? (
          <Pressable
            onPress={() => removeMember(m)}
            disabled={busyUserId === m.user_id}
            accessibilityRole="button"
            accessibilityLabel={`Remove ${name} from the channel`}
            hitSlop={8}
            style={{
              minHeight: MIN_TOUCH,
              minWidth: MIN_TOUCH,
              alignItems: 'center',
              justifyContent: 'center',
              opacity: busyUserId === m.user_id ? 0.5 : 1,
            }}
          >
            <Text style={{ color: theme.danger, fontSize: 13, fontWeight: '600' }}>Remove</Text>
          </Pressable>
        ) : null}
      </View>
    )
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title={`Members${members.length ? ` (${members.length})` : ''}`}
        subtitle={channel?.name ? `#${channel.name}` : undefined}
        onBack={() => navigation.goBack()}
        right={
          canManage ? (
            <Pressable
              onPress={() => setAdding((v) => !v)}
              accessibilityRole="button"
              accessibilityLabel={adding ? 'Close add member' : 'Add member'}
              accessibilityState={{ expanded: adding }}
              hitSlop={8}
              style={{ minHeight: MIN_TOUCH, minWidth: MIN_TOUCH, alignItems: 'center', justifyContent: 'center' }}
            >
              <Text style={{ color: theme.accent, fontSize: 14, fontWeight: '600' }}>{adding ? 'Done' : 'Add'}</Text>
            </Pressable>
          ) : undefined
        }
      />

      {adding && canManage ? (
        <View style={{ paddingHorizontal: 16, paddingTop: 12, paddingBottom: 4 }}>
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
              paddingVertical: 12,
              minHeight: MIN_TOUCH,
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
          refreshControl={
            <RefreshControl refreshing={refreshing} onRefresh={() => load('refresh')} tintColor={theme.accent} />
          }
          ListEmptyComponent={<EmptyState title="No members in this channel" />}
        />
      )}
    </SafeAreaView>
  )
}
