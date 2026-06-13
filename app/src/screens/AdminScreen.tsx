import React, { useEffect, useState, useCallback } from 'react'
import {
  View,
  Text,
  TextInput,
  Pressable,
  SafeAreaView,
  ScrollView,
  FlatList,
  Alert,
  ActivityIndicator,
} from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import { api } from '../api/client'

// --- Shapes returned by the raw /admin/* endpoints ----------------------

interface AdminStats {
  total_users?: number
  active_users?: number
  total_workspaces?: number
  total_channels?: number
  total_messages?: number
  [key: string]: number | string | undefined
}

interface AdminUser {
  id: string
  email: string
  username: string
  full_name: string
  is_active: boolean
}

interface AdminInvitation {
  id: string
  email: string
  role: string
  accepted?: boolean
  created_at?: string
}

type Tab = 'stats' | 'users' | 'invitations'

// --- Presentational helpers ---------------------------------------------

function TabBar({ tab, setTab }: { tab: Tab; setTab: (t: Tab) => void }) {
  const tabs: { key: Tab; label: string }[] = [
    { key: 'stats', label: 'Stats' },
    { key: 'users', label: 'Users' },
    { key: 'invitations', label: 'Invitations' },
  ]
  return (
    <View
      style={{
        flexDirection: 'row',
        borderBottomWidth: 1,
        borderBottomColor: theme.border,
      }}
    >
      {tabs.map((t) => {
        const active = t.key === tab
        return (
          <Pressable
            key={t.key}
            onPress={() => setTab(t.key)}
            style={{
              flex: 1,
              paddingVertical: 12,
              alignItems: 'center',
              borderBottomWidth: 2,
              borderBottomColor: active ? theme.primary : 'transparent',
            }}
          >
            <Text
              style={{
                color: active ? theme.text : theme.textMuted,
                fontSize: 14,
                fontWeight: active ? '700' : '500',
              }}
            >
              {t.label}
            </Text>
          </Pressable>
        )
      })}
    </View>
  )
}

function StatCard({ label, value }: { label: string; value: number | string }) {
  return (
    <View
      style={{
        flexGrow: 1,
        flexBasis: '47%',
        backgroundColor: theme.surface,
        borderWidth: 1,
        borderColor: theme.border,
        borderRadius: 12,
        padding: 16,
      }}
    >
      <Text style={{ color: theme.text, fontSize: 26, fontWeight: '700' }}>{value}</Text>
      <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 4 }}>{label}</Text>
    </View>
  )
}

function prettyLabel(key: string): string {
  return key
    .replace(/_/g, ' ')
    .replace(/\b\w/g, (c) => c.toUpperCase())
}

// --- Screen --------------------------------------------------------------

export default function AdminScreen({ navigation }: { navigation: any; route: any }) {
  const [tab, setTab] = useState<Tab>('stats')
  const [forbidden, setForbidden] = useState(false)
  const [initialLoading, setInitialLoading] = useState(true)

  // Stats
  const [stats, setStats] = useState<AdminStats | null>(null)

  // Users
  const [users, setUsers] = useState<AdminUser[]>([])
  const [usersLoaded, setUsersLoaded] = useState(false)
  const [pendingUserId, setPendingUserId] = useState<string | null>(null)

  // Invitations
  const [invitations, setInvitations] = useState<AdminInvitation[]>([])
  const [invitesLoaded, setInvitesLoaded] = useState(false)
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<'member' | 'admin'>('member')
  const [creatingInvite, setCreatingInvite] = useState(false)

  const handleError = useCallback((err: unknown, fallback: string) => {
    const msg = err instanceof Error ? err.message : fallback
    if (/403|forbidden|admin/i.test(msg)) {
      setForbidden(true)
      return
    }
    Alert.alert('Error', msg)
  }, [])

  // Stats drive the initial access check.
  useEffect(() => {
    let cancelled = false
    api
      .get<AdminStats>('/admin/stats')
      .then((res) => {
        if (cancelled) return
        setStats(res.data ?? {})
      })
      .catch((err) => {
        if (cancelled) return
        handleError(err, 'Failed to load stats')
      })
      .finally(() => {
        if (!cancelled) setInitialLoading(false)
      })
    return () => {
      cancelled = true
    }
  }, [handleError])

  const loadUsers = useCallback(async () => {
    try {
      const res = await api.get<AdminUser[]>('/admin/users')
      setUsers(res.data ?? [])
      setUsersLoaded(true)
    } catch (err) {
      handleError(err, 'Failed to load users')
    }
  }, [handleError])

  const loadInvitations = useCallback(async () => {
    try {
      const res = await api.get<AdminInvitation[]>('/admin/invitations')
      setInvitations(res.data ?? [])
      setInvitesLoaded(true)
    } catch (err) {
      handleError(err, 'Failed to load invitations')
    }
  }, [handleError])

  useEffect(() => {
    if (forbidden) return
    if (tab === 'users' && !usersLoaded) loadUsers()
    if (tab === 'invitations' && !invitesLoaded) loadInvitations()
  }, [tab, forbidden, usersLoaded, invitesLoaded, loadUsers, loadInvitations])

  const toggleUserActive = async (u: AdminUser) => {
    setPendingUserId(u.id)
    try {
      const res = await api.patch<AdminUser>(`/admin/users/${u.id}`, { is_active: !u.is_active })
      const next = res.data
      setUsers((prev) =>
        prev.map((x) => (x.id === u.id ? { ...x, is_active: next?.is_active ?? !u.is_active } : x)),
      )
    } catch (err) {
      handleError(err, 'Failed to update user')
    } finally {
      setPendingUserId(null)
    }
  }

  const createInvitation = async () => {
    const email = inviteEmail.trim()
    if (!email || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      Alert.alert('Validation', 'Enter a valid email address')
      return
    }
    setCreatingInvite(true)
    try {
      const res = await api.post<AdminInvitation>('/admin/invitations', { email, role: inviteRole })
      if (res.data) setInvitations((prev) => [res.data, ...prev])
      setInviteEmail('')
      Alert.alert('Invitation sent', `Invited ${email} as ${inviteRole}`)
    } catch (err) {
      handleError(err, 'Failed to create invitation')
    } finally {
      setCreatingInvite(false)
    }
  }

  // --- Header (shared) ---
  const Header = (
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
      <Pressable onPress={() => navigation.goBack()} hitSlop={12} style={{ marginRight: 12 }}>
        <Text style={{ color: theme.accent, fontSize: 16 }}>‹ Back</Text>
      </Pressable>
      <Text style={{ color: theme.text, fontSize: 17, fontWeight: '700', flex: 1 }}>Admin</Text>
    </View>
  )

  // --- Loading / forbidden states ---
  if (initialLoading) {
    return (
      <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
        {Header}
        <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
          <ActivityIndicator color={theme.accent} />
        </View>
      </SafeAreaView>
    )
  }

  if (forbidden) {
    return (
      <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
        {Header}
        <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center', padding: 32 }}>
          <Text style={{ color: theme.text, fontSize: 18, fontWeight: '700', marginBottom: 8 }}>
            Admin access required
          </Text>
          <Text style={{ color: theme.textMuted, fontSize: 14, textAlign: 'center' }}>
            You do not have permission to view the admin panel.
          </Text>
        </View>
      </SafeAreaView>
    )
  }

  // --- Tab content ---
  const statsEntries = stats ? Object.entries(stats).filter(([, v]) => v !== undefined && v !== null) : []

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      {Header}
      <TabBar tab={tab} setTab={setTab} />

      {tab === 'stats' && (
        <ScrollView contentContainerStyle={{ padding: 16 }}>
          {statsEntries.length === 0 ? (
            <Text style={{ color: theme.textMuted, fontSize: 14, textAlign: 'center', marginTop: 24 }}>
              No stats available.
            </Text>
          ) : (
            <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 12 }}>
              {statsEntries.map(([key, value]) => (
                <StatCard key={key} label={prettyLabel(key)} value={value as number | string} />
              ))}
            </View>
          )}
        </ScrollView>
      )}

      {tab === 'users' && (
        <FlatList
          data={users}
          keyExtractor={(u) => u.id}
          contentContainerStyle={{ padding: 16 }}
          ListEmptyComponent={
            usersLoaded ? (
              <Text style={{ color: theme.textMuted, fontSize: 14, textAlign: 'center', marginTop: 24 }}>
                No users found.
              </Text>
            ) : (
              <ActivityIndicator color={theme.accent} style={{ marginTop: 24 }} />
            )
          }
          renderItem={({ item: u }) => (
            <View
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                backgroundColor: theme.surface,
                borderWidth: 1,
                borderColor: theme.border,
                borderRadius: 12,
                padding: 12,
                marginBottom: 10,
              }}
            >
              <View
                style={{
                  width: 40,
                  height: 40,
                  borderRadius: 20,
                  backgroundColor: avatarColor(u.id),
                  alignItems: 'center',
                  justifyContent: 'center',
                  marginRight: 12,
                }}
              >
                <Text style={{ color: '#fff', fontWeight: '700' }}>
                  {(u.full_name || u.username || '?').charAt(0).toUpperCase()}
                </Text>
              </View>
              <View style={{ flex: 1 }}>
                <Text style={{ color: theme.text, fontSize: 15, fontWeight: '600' }}>
                  {u.full_name || u.username}
                </Text>
                <Text style={{ color: theme.textMuted, fontSize: 12 }}>{u.email}</Text>
                <Text
                  style={{
                    color: u.is_active ? theme.success : theme.textFaint,
                    fontSize: 11,
                    marginTop: 2,
                  }}
                >
                  {u.is_active ? 'Active' : 'Deactivated'}
                </Text>
              </View>
              <Pressable
                onPress={() => toggleUserActive(u)}
                disabled={pendingUserId === u.id}
                style={{
                  borderWidth: 1,
                  borderColor: u.is_active ? theme.danger : theme.success,
                  borderRadius: 8,
                  paddingHorizontal: 12,
                  paddingVertical: 8,
                  minWidth: 96,
                  alignItems: 'center',
                  opacity: pendingUserId === u.id ? 0.5 : 1,
                }}
              >
                {pendingUserId === u.id ? (
                  <ActivityIndicator color={theme.body} />
                ) : (
                  <Text
                    style={{
                      color: u.is_active ? theme.danger : theme.success,
                      fontSize: 13,
                      fontWeight: '600',
                    }}
                  >
                    {u.is_active ? 'Deactivate' : 'Activate'}
                  </Text>
                )}
              </Pressable>
            </View>
          )}
        />
      )}

      {tab === 'invitations' && (
        <FlatList
          data={invitations}
          keyExtractor={(i) => i.id}
          contentContainerStyle={{ padding: 16 }}
          ListHeaderComponent={
            <View
              style={{
                backgroundColor: theme.surface,
                borderWidth: 1,
                borderColor: theme.border,
                borderRadius: 12,
                padding: 16,
                marginBottom: 16,
              }}
            >
              <Text style={{ color: theme.text, fontSize: 15, fontWeight: '700', marginBottom: 12 }}>
                Invite a new member
              </Text>
              <TextInput
                value={inviteEmail}
                onChangeText={setInviteEmail}
                placeholder="email@example.com"
                placeholderTextColor={theme.textFaint}
                keyboardType="email-address"
                autoCapitalize="none"
                style={{
                  backgroundColor: theme.bg,
                  borderWidth: 1,
                  borderColor: theme.borderStrong,
                  borderRadius: 10,
                  paddingHorizontal: 12,
                  paddingVertical: 10,
                  color: theme.text,
                  fontSize: 15,
                  marginBottom: 12,
                }}
              />
              <View style={{ flexDirection: 'row', marginBottom: 12 }}>
                {(['member', 'admin'] as const).map((r) => {
                  const active = inviteRole === r
                  return (
                    <Pressable
                      key={r}
                      onPress={() => setInviteRole(r)}
                      style={{
                        flex: 1,
                        paddingVertical: 10,
                        alignItems: 'center',
                        borderWidth: 1,
                        borderColor: active ? theme.primary : theme.borderStrong,
                        backgroundColor: active ? theme.primary : 'transparent',
                        borderRadius: 10,
                        marginRight: r === 'member' ? 8 : 0,
                      }}
                    >
                      <Text
                        style={{
                          color: active ? theme.primaryText : theme.body,
                          fontWeight: '600',
                          fontSize: 14,
                        }}
                      >
                        {r === 'member' ? 'Member' : 'Admin'}
                      </Text>
                    </Pressable>
                  )
                })}
              </View>
              <Pressable
                onPress={createInvitation}
                disabled={creatingInvite}
                style={{
                  backgroundColor: theme.primary,
                  borderRadius: 10,
                  paddingVertical: 12,
                  alignItems: 'center',
                  opacity: creatingInvite ? 0.6 : 1,
                }}
              >
                {creatingInvite ? (
                  <ActivityIndicator color={theme.primaryText} />
                ) : (
                  <Text style={{ color: theme.primaryText, fontSize: 15, fontWeight: '600' }}>
                    Send invitation
                  </Text>
                )}
              </Pressable>
            </View>
          }
          ListEmptyComponent={
            invitesLoaded ? (
              <Text style={{ color: theme.textMuted, fontSize: 14, textAlign: 'center' }}>
                No invitations yet.
              </Text>
            ) : (
              <ActivityIndicator color={theme.accent} />
            )
          }
          renderItem={({ item: i }) => (
            <View
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                backgroundColor: theme.surface,
                borderWidth: 1,
                borderColor: theme.border,
                borderRadius: 12,
                padding: 12,
                marginBottom: 10,
              }}
            >
              <View style={{ flex: 1 }}>
                <Text style={{ color: theme.text, fontSize: 15, fontWeight: '600' }}>{i.email}</Text>
                <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 2 }}>
                  {prettyLabel(i.role)}
                </Text>
              </View>
              <Text
                style={{
                  color: i.accepted ? theme.success : theme.warning,
                  fontSize: 12,
                  fontWeight: '600',
                }}
              >
                {i.accepted ? 'Accepted' : 'Pending'}
              </Text>
            </View>
          )}
        />
      )}
    </SafeAreaView>
  )
}
