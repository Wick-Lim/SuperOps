import React, { useEffect, useState } from 'react'
import { View, Text, Pressable, SafeAreaView, FlatList, ActivityIndicator } from 'react-native'
import { theme, avatarColor, presenceColor } from '../lib/theme'
import type { ChannelMemberView, PresenceStatus } from '../lib/types'
import { channelApi } from '../api/channels'
import { workspaceApi } from '../api/workspaces'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUserStore, displayName } from '../stores/userStore'

function roleLabel(role: string): string {
  if (!role) return ''
  return role.charAt(0).toUpperCase() + role.slice(1)
}

export default function MembersScreen({ navigation, route }: { navigation: any; route: any }) {
  const channelId: string | undefined = route.params?.channelId
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [members, setMembers] = useState<ChannelMemberView[]>([])
  const [presence, setPresence] = useState<Record<string, PresenceStatus>>({})
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)

  const load = () => {
    if (!activeWorkspace || !channelId) {
      setError('Missing channel context')
      setLoading(false)
      return
    }
    setLoading(true)
    setError(null)
    channelApi
      .listMembers(activeWorkspace.id, channelId)
      .then((res) => {
        const list = res.data ?? []
        setMembers(list)
        ensureUsers(list.map((m) => m.user_id))
      })
      .catch((err) => setError(err instanceof Error ? err.message : 'Failed to load members'))
      .finally(() => setLoading(false))
  }

  useEffect(() => {
    load()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeWorkspace?.id, channelId])

  useEffect(() => {
    if (!activeWorkspace) return
    workspaceApi
      .presence(activeWorkspace.id)
      .then((res) => setPresence(res.data ?? {}))
      .catch(() => {})
  }, [activeWorkspace?.id])

  const renderMember = ({ item: m }: { item: ChannelMemberView }) => {
    const name = displayName(users, m.user_id)
    const status: PresenceStatus = presence[m.user_id] ?? 'offline'
    const dotColor = presenceColor[status] ?? presenceColor.offline
    return (
      <View
        style={{
          flexDirection: 'row',
          alignItems: 'center',
          paddingHorizontal: 16,
          paddingVertical: 12,
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
            <Text style={{ color: theme.text, fontWeight: '700', fontSize: 15 }}>
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
          <Text style={{ color: theme.body, fontSize: 15, fontWeight: '500' }}>{name}</Text>
          <Text style={{ color: theme.textFaint, fontSize: 12, marginTop: 1, textTransform: 'capitalize' }}>
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
            }}
          >
            <Text style={{ color: theme.textMuted, fontSize: 12, fontWeight: '600' }}>{roleLabel(m.role)}</Text>
          </View>
        ) : null}
      </View>
    )
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      {/* Header */}
      <View
        style={{
          height: 56,
          paddingHorizontal: 16,
          flexDirection: 'row',
          alignItems: 'center',
          borderBottomWidth: 1,
          borderBottomColor: theme.border,
        }}
      >
        <Pressable onPress={() => navigation.goBack()} hitSlop={8} style={{ marginRight: 12 }}>
          <Text style={{ color: theme.accent, fontSize: 16 }}>Back</Text>
        </Pressable>
        <Text style={{ color: theme.text, fontSize: 17, fontWeight: '600', flex: 1 }}>
          Members{members.length ? ` (${members.length})` : ''}
        </Text>
      </View>

      {loading ? (
        <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
          <ActivityIndicator color={theme.primary} />
        </View>
      ) : error ? (
        <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center', padding: 24 }}>
          <Text style={{ color: theme.danger, fontSize: 14, textAlign: 'center' }}>{error}</Text>
          <Pressable onPress={load} style={{ marginTop: 12 }}>
            <Text style={{ color: theme.accent, fontSize: 14 }}>Try again</Text>
          </Pressable>
        </View>
      ) : (
        <FlatList
          data={members}
          keyExtractor={(m) => m.user_id}
          renderItem={renderMember}
          ListEmptyComponent={
            <Text style={{ color: theme.textFaint, fontSize: 14, textAlign: 'center', paddingTop: 32 }}>
              No members in this channel.
            </Text>
          }
        />
      )}
    </SafeAreaView>
  )
}
