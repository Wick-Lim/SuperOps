import React, { useCallback, useEffect, useState } from 'react'
import {
  View,
  Text,
  Pressable,
  SafeAreaView,
  ScrollView,
  FlatList,
  Alert,
  RefreshControl,
} from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import {
  adminApi,
  type AdminInvitation,
  type AdminStats,
  type AdminUser,
  type AuditLogEntry,
  type CreatedInvitation,
} from '../api/admin'
import { workspaceApi } from '../api/workspaces'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { errorMessage, isApiError } from '../api/client'
import type { Workspace, WorkspaceRole } from '../lib/types'
import {
  Button,
  Chip,
  EmptyState,
  ErrorState,
  Field,
  ListFooter,
  LoadingState,
  MIN_TOUCH,
  ScreenHeader,
} from './internal/ui'

type Tab = 'stats' | 'users' | 'invitations' | 'audit'

const TABS: Array<{ key: Tab; label: string }> = [
  { key: 'stats', label: 'Stats' },
  { key: 'users', label: 'Users' },
  { key: 'invitations', label: 'Invites' },
  { key: 'audit', label: 'Audit' },
]

const INVITE_ROLES: Array<Exclude<WorkspaceRole, 'owner'>> = ['admin', 'member', 'guest']

const STAT_LABELS: Array<{ key: keyof AdminStats; label: string }> = [
  { key: 'users', label: 'Members' },
  { key: 'workspaces', label: 'Workspaces' },
  { key: 'channels', label: 'Channels' },
  { key: 'messages', label: 'Messages' },
]

function TabBar({ tab, setTab }: { tab: Tab; setTab: (t: Tab) => void }) {
  return (
    <View
      accessibilityRole="tablist"
      style={{ flexDirection: 'row', borderBottomWidth: 1, borderBottomColor: theme.border }}
    >
      {TABS.map((t) => {
        const active = t.key === tab
        return (
          <Pressable
            key={t.key}
            onPress={() => setTab(t.key)}
            accessibilityRole="tab"
            accessibilityLabel={t.label}
            accessibilityState={{ selected: active }}
            style={{
              flex: 1,
              paddingVertical: 12,
              minHeight: MIN_TOUCH,
              alignItems: 'center',
              justifyContent: 'center',
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

function StatCard({ label, value }: { label: string; value: number }) {
  return (
    <View
      accessible
      accessibilityLabel={`${value} ${label}`}
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

function statusColor(status: string): string {
  if (status === 'accepted') return theme.success
  if (status === 'expired' || status === 'revoked') return theme.textMuted
  return theme.warning
}

function shortDate(iso: string): string {
  const d = new Date(iso)
  return Number.isNaN(d.getTime()) ? '' : d.toLocaleDateString()
}

export default function AdminScreen({ navigation }: { navigation: any; route: any }) {
  const [tab, setTab] = useState<Tab>('stats')
  const [forbidden, setForbidden] = useState(false)
  const [refreshing, setRefreshing] = useState(false)

  const storeWorkspaces = useWorkspaceStore((s) => s.workspaces)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const [workspaces, setWorkspaces] = useState<Workspace[]>(storeWorkspaces)
  const [targetWorkspaceId, setTargetWorkspaceId] = useState<string | null>(
    activeWorkspace?.id ?? storeWorkspaces[0]?.id ?? null,
  )

  // Stats
  const [stats, setStats] = useState<AdminStats | null>(null)
  const [statsError, setStatsError] = useState<string | null>(null)
  const [statsLoading, setStatsLoading] = useState(true)

  // Users
  const [users, setUsers] = useState<AdminUser[]>([])
  const [usersLoaded, setUsersLoaded] = useState(false)
  const [usersError, setUsersError] = useState<string | null>(null)
  const [pendingUserId, setPendingUserId] = useState<string | null>(null)
  const [expandedUserId, setExpandedUserId] = useState<string | null>(null)

  // Invitations
  const [invitations, setInvitations] = useState<AdminInvitation[]>([])
  const [invitesLoaded, setInvitesLoaded] = useState(false)
  const [invitesError, setInvitesError] = useState<string | null>(null)
  const [inviteEmail, setInviteEmail] = useState('')
  const [inviteRole, setInviteRole] = useState<Exclude<WorkspaceRole, 'owner'>>('member')
  const [creatingInvite, setCreatingInvite] = useState(false)
  const [lastCreated, setLastCreated] = useState<CreatedInvitation | null>(null)

  // Audit
  const [audit, setAudit] = useState<AuditLogEntry[]>([])
  const [auditCursor, setAuditCursor] = useState<string | undefined>(undefined)
  const [auditHasMore, setAuditHasMore] = useState(false)
  const [auditLoading, setAuditLoading] = useState(false)
  const [auditLoaded, setAuditLoaded] = useState(false)
  const [auditError, setAuditError] = useState<string | null>(null)

  /** 403 is the whole-screen answer; everything else is a per-section error. */
  const asError = useCallback((err: unknown, fallback: string): string => {
    if (isApiError(err) && err.status === 403) {
      setForbidden(true)
      return 'Admin access required'
    }
    return errorMessage(err, fallback)
  }, [])

  const loadStats = useCallback(async () => {
    setStatsLoading(true)
    setStatsError(null)
    try {
      const res = await adminApi.stats()
      setStats(res.data)
    } catch (err) {
      setStatsError(asError(err, 'Failed to load stats'))
    } finally {
      setStatsLoading(false)
    }
  }, [asError])

  useEffect(() => {
    void loadStats()
  }, [loadStats])

  // The workspace picker needs the full list, not just the active one.
  useEffect(() => {
    if (storeWorkspaces.length > 0) return
    workspaceApi
      .list()
      .then((res) => {
        setWorkspaces(res.data ?? [])
        setTargetWorkspaceId((cur) => cur ?? res.data?.[0]?.id ?? null)
      })
      .catch(() => {
        /* the picker simply stays empty; create/role actions explain why */
      })
  }, [storeWorkspaces.length])

  useEffect(() => {
    if (storeWorkspaces.length > 0) setWorkspaces(storeWorkspaces)
  }, [storeWorkspaces])

  const loadUsers = useCallback(async () => {
    setUsersError(null)
    try {
      const res = await adminApi.listUsers()
      setUsers(res.data ?? [])
      setUsersLoaded(true)
    } catch (err) {
      setUsersError(asError(err, 'Failed to load users'))
      setUsersLoaded(true)
    }
  }, [asError])

  const loadInvitations = useCallback(async () => {
    setInvitesError(null)
    try {
      const res = await adminApi.listInvitations()
      setInvitations(res.data ?? [])
      setInvitesLoaded(true)
    } catch (err) {
      setInvitesError(asError(err, 'Failed to load invitations'))
      setInvitesLoaded(true)
    }
  }, [asError])

  const loadAudit = useCallback(
    async (cursor?: string) => {
      setAuditLoading(true)
      setAuditError(null)
      try {
        const res = await adminApi.auditLogs(cursor, 30)
        const page = res.data ?? []
        setAudit((prev) => (cursor ? [...prev, ...page] : page))
        setAuditCursor(res.meta?.cursor)
        setAuditHasMore(!!res.meta?.has_more && !!res.meta?.cursor)
        setAuditLoaded(true)
      } catch (err) {
        setAuditError(asError(err, 'Failed to load audit log'))
        setAuditLoaded(true)
      } finally {
        setAuditLoading(false)
      }
    },
    [asError],
  )

  useEffect(() => {
    if (forbidden) return
    if (tab === 'users' && !usersLoaded) void loadUsers()
    if (tab === 'invitations' && !invitesLoaded) void loadInvitations()
    if (tab === 'audit' && !auditLoaded) void loadAudit()
  }, [tab, forbidden, usersLoaded, invitesLoaded, auditLoaded, loadUsers, loadInvitations, loadAudit])

  const toggleUserActive = async (u: AdminUser) => {
    setPendingUserId(u.id)
    try {
      // The response is {message}, not the user — apply the change we asked for.
      await adminApi.updateUser(u.id, { is_active: !u.is_active })
      setUsers((prev) => prev.map((x) => (x.id === u.id ? { ...x, is_active: !u.is_active } : x)))
    } catch (err) {
      Alert.alert('Error', asError(err, 'Failed to update user'))
    } finally {
      setPendingUserId(null)
    }
  }

  const setUserRole = async (u: AdminUser, role: Exclude<WorkspaceRole, 'owner'>) => {
    if (!targetWorkspaceId) {
      Alert.alert('Pick a workspace', 'A role only means something inside a workspace.')
      return
    }
    setPendingUserId(u.id)
    try {
      // workspace_id is REQUIRED whenever role is set.
      await adminApi.updateUser(u.id, { role, workspace_id: targetWorkspaceId })
      Alert.alert('Saved', `${u.full_name || u.username} is now ${role} in this workspace.`)
      setExpandedUserId(null)
    } catch (err) {
      Alert.alert('Error', asError(err, 'Failed to change role'))
    } finally {
      setPendingUserId(null)
    }
  }

  const createInvitation = async () => {
    const email = inviteEmail.trim().toLowerCase()
    if (!email || !/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(email)) {
      Alert.alert('Validation', 'Enter a valid email address')
      return
    }
    if (!targetWorkspaceId) {
      Alert.alert('Pick a workspace', 'Choose which workspace this invitation is for.')
      return
    }
    setCreatingInvite(true)
    try {
      const res = await adminApi.createInvitation({
        workspace_id: targetWorkspaceId,
        email,
        role: inviteRole,
      })
      // The response is a CreatedInvitation ({id, invite_url, ...}), NOT an
      // invitation row: unshifting it into the list used to render a row with
      // `role === undefined` and crash the tree. Refetch instead.
      setLastCreated(res.data)
      setInviteEmail('')
      void loadInvitations()
    } catch (err) {
      Alert.alert('Error', asError(err, 'Failed to create invitation'))
    } finally {
      setCreatingInvite(false)
    }
  }

  const workspaceName = (id: string) => workspaces.find((w) => w.id === id)?.name ?? 'workspace'

  /** Drives the pull-to-refresh spinner for whichever tab is showing. */
  const refresh = (fn: () => Promise<void>) => {
    setRefreshing(true)
    void fn().finally(() => setRefreshing(false))
  }

  const header = (
    <ScreenHeader title="Admin" onBack={() => navigation.goBack()} subtitle="Workspaces you administer" />
  )

  if (forbidden) {
    return (
      <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
        {header}
        <EmptyState
          title="Admin access required"
          body="You need to be an owner or admin of a workspace to open this panel."
        />
      </SafeAreaView>
    )
  }

  const workspacePicker = (
    <View style={{ marginBottom: 12 }}>
      <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 6 }}>Workspace</Text>
      <ScrollView horizontal showsHorizontalScrollIndicator={false} contentContainerStyle={{ gap: 8 }}>
        {workspaces.length === 0 ? (
          <Text style={{ color: theme.textMuted, fontSize: 13 }}>No workspaces available.</Text>
        ) : (
          workspaces.map((w) => (
            <Chip
              key={w.id}
              label={w.name}
              selected={w.id === targetWorkspaceId}
              onPress={() => setTargetWorkspaceId(w.id)}
              accessibilityLabel={`Workspace ${w.name}`}
            />
          ))
        )}
      </ScrollView>
    </View>
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      {header}
      <TabBar tab={tab} setTab={setTab} />

      {tab === 'stats' &&
        (statsLoading ? (
          <LoadingState label="Loading stats" />
        ) : statsError ? (
          <ErrorState message={statsError} onRetry={loadStats} />
        ) : (
          <ScrollView
            contentContainerStyle={{ padding: 16 }}
            refreshControl={
              <RefreshControl
                refreshing={refreshing}
                onRefresh={() => refresh(loadStats)}
                tintColor={theme.accent}
              />
            }
          >
            <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 12 }}>
              {STAT_LABELS.map(({ key, label }) => (
                <StatCard key={key} label={label} value={stats?.[key] ?? 0} />
              ))}
            </View>
          </ScrollView>
        ))}

      {tab === 'users' && (
        <FlatList
          data={users}
          keyExtractor={(u) => u.id}
          contentContainerStyle={{ padding: 16 }}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => refresh(loadUsers)}
              tintColor={theme.accent}
            />
          }
          ListHeaderComponent={usersError ? null : workspacePicker}
          ListEmptyComponent={
            usersError ? (
              <ErrorState message={usersError} onRetry={loadUsers} />
            ) : usersLoaded ? (
              <EmptyState title="No users found" />
            ) : (
              <LoadingState label="Loading users" />
            )
          }
          renderItem={({ item: u }) => {
            const busy = pendingUserId === u.id
            const expanded = expandedUserId === u.id
            return (
              <View
                style={{
                  backgroundColor: theme.surface,
                  borderWidth: 1,
                  borderColor: theme.border,
                  borderRadius: 12,
                  padding: 12,
                  marginBottom: 10,
                }}
              >
                <View style={{ flexDirection: 'row', alignItems: 'center' }}>
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
                      {u.is_bot ? '  ·  bot' : ''}
                    </Text>
                    <Text style={{ color: theme.textMuted, fontSize: 12 }}>{u.email}</Text>
                    <Text
                      style={{
                        color: u.is_active ? theme.success : theme.textMuted,
                        fontSize: 11,
                        marginTop: 2,
                      }}
                    >
                      {u.is_active ? 'Active' : 'Deactivated'}
                    </Text>
                  </View>
                  <Pressable
                    onPress={() => toggleUserActive(u)}
                    disabled={busy}
                    accessibilityRole="button"
                    accessibilityLabel={`${u.is_active ? 'Deactivate' : 'Activate'} ${u.full_name || u.username}`}
                    accessibilityState={{ disabled: busy, busy }}
                    style={{
                      borderWidth: 1,
                      borderColor: u.is_active ? theme.danger : theme.success,
                      borderRadius: 8,
                      paddingHorizontal: 12,
                      minHeight: MIN_TOUCH,
                      minWidth: 100,
                      alignItems: 'center',
                      justifyContent: 'center',
                      opacity: busy ? 0.5 : 1,
                    }}
                  >
                    <Text
                      style={{
                        color: u.is_active ? theme.danger : theme.success,
                        fontSize: 13,
                        fontWeight: '600',
                      }}
                    >
                      {u.is_active ? 'Deactivate' : 'Activate'}
                    </Text>
                  </Pressable>
                </View>

                <Pressable
                  onPress={() => setExpandedUserId(expanded ? null : u.id)}
                  accessibilityRole="button"
                  accessibilityLabel={expanded ? 'Hide role options' : `Change role for ${u.full_name || u.username}`}
                  accessibilityState={{ expanded }}
                  hitSlop={8}
                  style={{ marginTop: 10, minHeight: 32, justifyContent: 'center' }}
                >
                  <Text style={{ color: theme.accent, fontSize: 13 }}>
                    {expanded ? 'Hide role' : 'Change role…'}
                  </Text>
                </Pressable>

                {expanded ? (
                  <View style={{ marginTop: 8 }}>
                    <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 8 }}>
                      Role in {targetWorkspaceId ? workspaceName(targetWorkspaceId) : 'no workspace selected'}
                    </Text>
                    <View style={{ flexDirection: 'row', gap: 8 }}>
                      {INVITE_ROLES.map((r) => (
                        <Chip
                          key={r}
                          label={r}
                          selected={false}
                          onPress={() => setUserRole(u, r)}
                          accessibilityLabel={`Set role ${r}`}
                        />
                      ))}
                    </View>
                  </View>
                ) : null}
              </View>
            )
          }}
        />
      )}

      {tab === 'invitations' && (
        <FlatList
          data={invitations}
          keyExtractor={(i) => i.id}
          contentContainerStyle={{ padding: 16 }}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => refresh(loadInvitations)}
              tintColor={theme.accent}
            />
          }
          ListHeaderComponent={
            <View>
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
                <Text
                  accessibilityRole="header"
                  style={{ color: theme.text, fontSize: 15, fontWeight: '700', marginBottom: 12 }}
                >
                  Invite a new member
                </Text>
                {workspacePicker}
                <Field
                  label="Email"
                  value={inviteEmail}
                  onChangeText={setInviteEmail}
                  placeholder="email@example.com"
                  keyboardType="email-address"
                />
                <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 6 }}>Role</Text>
                <View style={{ flexDirection: 'row', gap: 8, marginBottom: 14 }}>
                  {INVITE_ROLES.map((r) => (
                    <Chip
                      key={r}
                      label={r}
                      selected={inviteRole === r}
                      onPress={() => setInviteRole(r)}
                      accessibilityLabel={`Invite as ${r}`}
                    />
                  ))}
                </View>
                <Button label="Send invitation" onPress={createInvitation} loading={creatingInvite} />
              </View>

              {lastCreated ? (
                <View
                  style={{
                    backgroundColor: theme.surface,
                    borderWidth: 1,
                    borderColor: theme.primary,
                    borderRadius: 12,
                    padding: 16,
                    marginBottom: 16,
                  }}
                >
                  <Text style={{ color: theme.text, fontSize: 14, fontWeight: '700' }}>
                    Invite link for {lastCreated.email}
                  </Text>
                  <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 4, marginBottom: 8 }}>
                    Shown once — the server stores only a hash. SuperOps does not send email.
                  </Text>
                  <Text selectable style={{ color: theme.accent, fontSize: 13, fontFamily: 'Courier' }}>
                    {lastCreated.invite_url}
                  </Text>
                  <View style={{ height: 12 }} />
                  <Button label="Dismiss" onPress={() => setLastCreated(null)} variant="ghost" />
                </View>
              ) : null}
            </View>
          }
          ListEmptyComponent={
            invitesError ? (
              <ErrorState message={invitesError} onRetry={loadInvitations} />
            ) : invitesLoaded ? (
              <EmptyState title="No invitations yet" />
            ) : (
              <LoadingState label="Loading invitations" />
            )
          }
          renderItem={({ item: i }) => (
            <View
              accessible
              accessibilityLabel={`${i.email}, ${i.role}, ${i.status}`}
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
                  {i.role} · {workspaceName(i.workspace_id)}
                  {i.invited_by ? ` · by ${i.invited_by}` : ''}
                </Text>
                <Text style={{ color: theme.textMuted, fontSize: 11, marginTop: 2 }}>
                  expires {shortDate(i.expires_at)}
                </Text>
              </View>
              {/* The backend field is `status`; the screen used to read a
                  non-existent `accepted`, so everything said "Pending". */}
              <Text style={{ color: statusColor(i.status), fontSize: 12, fontWeight: '600' }}>
                {i.status.charAt(0).toUpperCase() + i.status.slice(1)}
              </Text>
            </View>
          )}
        />
      )}

      {tab === 'audit' && (
        <FlatList
          data={audit}
          keyExtractor={(a) => a.id}
          contentContainerStyle={{ padding: 16 }}
          refreshControl={
            <RefreshControl
              refreshing={refreshing}
              onRefresh={() => refresh(() => loadAudit())}
              tintColor={theme.accent}
            />
          }
          onEndReachedThreshold={0.4}
          onEndReached={() => {
            if (auditHasMore && !auditLoading) void loadAudit(auditCursor)
          }}
          ListEmptyComponent={
            auditError ? (
              <ErrorState message={auditError} onRetry={() => loadAudit()} />
            ) : auditLoaded ? (
              <EmptyState title="Nothing logged yet" />
            ) : (
              <LoadingState label="Loading audit log" />
            )
          }
          ListFooterComponent={
            <ListFooter
              loading={auditLoading && audit.length > 0}
              hasMore={auditHasMore}
              onLoadMore={() => loadAudit(auditCursor)}
            />
          }
          renderItem={({ item: a }) => (
            <View
              style={{
                backgroundColor: theme.surface,
                borderWidth: 1,
                borderColor: theme.border,
                borderRadius: 12,
                padding: 12,
                marginBottom: 10,
              }}
            >
              <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }}>{a.action}</Text>
              <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 2 }}>
                {a.resource_type}
                {a.resource_id ? ` · ${a.resource_id.slice(0, 8)}` : ''}
                {a.workspace_id ? ` · ${workspaceName(a.workspace_id)}` : ''}
              </Text>
              <Text style={{ color: theme.textMuted, fontSize: 11, marginTop: 4 }}>
                {new Date(a.created_at).toLocaleString()}
                {a.ip_address ? ` · ${a.ip_address}` : ''}
              </Text>
            </View>
          )}
        />
      )}
    </SafeAreaView>
  )
}
