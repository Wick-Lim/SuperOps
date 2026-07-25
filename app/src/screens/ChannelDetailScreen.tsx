import React, { useCallback, useEffect, useState } from 'react'
import { View, Text, Pressable, SafeAreaView, ScrollView, Switch, Alert } from 'react-native'
import { theme } from '../lib/theme'
import { channelApi } from '../api/channels'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useAuthStore } from '../stores/authStore'
import { errorMessage } from '../api/client'
import type { ChannelMemberView, ChannelNotificationPref } from '../lib/types'
import { useWorkspaceRole } from './internal/useWorkspaceRole'
import { useResponsive } from '../lib/responsive'
import {
  Button,
  Chip,
  CONTENT_MAX_WIDTH,
  ErrorState,
  LoadingState,
  ScreenHeader,
  Section,
  contentColumn,
} from './internal/ui'

const PREFS: Array<{ key: ChannelNotificationPref; label: string; hint: string }> = [
  { key: 'default', label: 'Default', hint: 'Follow the workspace default' },
  { key: 'all', label: 'All', hint: 'Notify for every message' },
  { key: 'mentions', label: 'Mentions', hint: 'Only when you are mentioned' },
  { key: 'none', label: 'None', hint: 'Never notify' },
]

function NavRow({
  label,
  detail,
  onPress,
}: {
  label: string
  detail?: string
  onPress: () => void
}) {
  const { minTouch } = useResponsive()
  return (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={detail ? `${label}, ${detail}` : label}
      style={{
        flexDirection: 'row',
        alignItems: 'center',
        minHeight: minTouch,
        paddingVertical: 10,
      }}
    >
      <Text style={{ color: theme.body, fontSize: 15, flex: 1 }}>{label}</Text>
      {detail ? <Text style={{ color: theme.textMuted, fontSize: 13, marginRight: 6 }}>{detail}</Text> : null}
      <Text style={{ color: theme.textMuted, fontSize: 18 }}>›</Text>
    </Pressable>
  )
}

/**
 * Channel hub: notification preferences (`PATCH /channels/{id}/prefs`), member
 * management, pins, scheduled messages, archive and leave — all backend routes
 * that previously had no entry point anywhere in the app.
 */
export default function ChannelDetailScreen({ navigation, route }: { navigation: any; route: any }) {
  const channelId: string = route.params?.channelId
  const channel = useChannelStore((s) => s.channels.find((c) => c.id === channelId) ?? null)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const me = useAuthStore((s) => s.user)
  const { isAdmin: workspaceIsAdmin } = useWorkspaceRole()
  const { tier, gutter, minTouch } = useResponsive()

  const [membership, setMembership] = useState<ChannelMemberView | null>(null)
  const [memberCount, setMemberCount] = useState<number | null>(null)
  const [loading, setLoading] = useState(true)
  const [error, setError] = useState<string | null>(null)
  const [saving, setSaving] = useState(false)
  const [busy, setBusy] = useState(false)

  const isDM = channel?.type === 'dm' || channel?.type === 'group_dm'

  const load = useCallback(async () => {
    if (!activeWorkspace || !channelId) {
      setError('Missing channel context')
      setLoading(false)
      return
    }
    setLoading(true)
    setError(null)
    try {
      const res = await channelApi.listMembers(activeWorkspace.id, channelId)
      const list = res.data ?? []
      setMemberCount(list.length)
      setMembership(list.find((m) => m.user_id === me?.id) ?? null)
    } catch (err) {
      setError(errorMessage(err, 'Failed to load channel details'))
    } finally {
      setLoading(false)
    }
  }, [activeWorkspace, channelId, me?.id])

  useEffect(() => {
    void load()
  }, [load])

  const savePrefs = async (patch: { muted?: boolean; notification_pref?: ChannelNotificationPref }) => {
    const previous = membership
    // Optimistic: the switch must not lag a round trip.
    if (membership) setMembership({ ...membership, ...patch })
    setSaving(true)
    try {
      const res = await channelApi.updatePrefs(channelId, patch)
      setMembership(res.data)
    } catch (err) {
      setMembership(previous)
      Alert.alert('Error', errorMessage(err, 'Could not save your preference'))
    } finally {
      setSaving(false)
    }
  }

  const toggleArchive = () => {
    if (!activeWorkspace || !channel) return
    const archiving = !channel.is_archived
    Alert.alert(
      archiving ? 'Archive channel' : 'Unarchive channel',
      archiving ? 'Members keep read access but nobody can post.' : 'The channel becomes writable again.',
      [
        { text: 'Cancel', style: 'cancel' },
        {
          text: archiving ? 'Archive' : 'Unarchive',
          style: archiving ? 'destructive' : 'default',
          onPress: async () => {
            setBusy(true)
            try {
              const res = archiving
                ? await channelApi.archive(activeWorkspace.id, channel.id)
                : await channelApi.unarchive(activeWorkspace.id, channel.id)
              useChannelStore.getState().upsertChannel(res.data)
            } catch (err) {
              Alert.alert('Error', errorMessage(err, 'Could not change the archive state'))
            } finally {
              setBusy(false)
            }
          },
        },
      ],
    )
  }

  const leave = () => {
    if (!activeWorkspace || !channel) return
    Alert.alert('Leave channel', `Leave ${channel.name ? `#${channel.name}` : 'this channel'}?`, [
      { text: 'Cancel', style: 'cancel' },
      {
        text: 'Leave',
        style: 'destructive',
        onPress: async () => {
          setBusy(true)
          try {
            await channelApi.leave(activeWorkspace.id, channel.id)
            useChannelStore.getState().removeChannel(channel.id)
            navigation.navigate('Workspace', {})
          } catch (err) {
            Alert.alert('Error', errorMessage(err, 'Could not leave the channel'))
          } finally {
            setBusy(false)
          }
        },
      },
    ])
  }

  const title = channel?.name ? `#${channel.name}` : isDM ? 'Conversation' : 'Channel'

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader
        title={title}
        subtitle={channel?.is_archived ? 'Archived' : channel?.topic || undefined}
        onBack={() => navigation.goBack()}
        maxWidth={CONTENT_MAX_WIDTH}
      />

      {loading ? (
        <LoadingState label="Loading channel details" />
      ) : error ? (
        <ErrorState message={error} onRetry={load} />
      ) : (
        <ScrollView
          contentContainerStyle={{
            ...contentColumn(),
            paddingHorizontal: gutter,
            paddingTop: 16,
            paddingBottom: 48,
          }}
        >
          {channel?.description ? (
            <Text style={{ color: theme.textMuted, fontSize: 14, lineHeight: 20, marginBottom: 20 }}>
              {channel.description}
            </Text>
          ) : null}

          <Section title="Notifications">
            <View
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                justifyContent: 'space-between',
                minHeight: minTouch,
              }}
            >
              <View style={{ flex: 1, paddingRight: 12 }}>
                <Text style={{ color: theme.text, fontSize: 15, fontWeight: '500' }}>Mute this channel</Text>
                <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 2 }}>
                  No badges and no push, whatever the preference below says
                </Text>
              </View>
              <Switch
                value={!!membership?.muted}
                disabled={!membership || saving}
                onValueChange={(v) => savePrefs({ muted: v })}
                accessibilityLabel="Mute this channel"
                trackColor={{ false: theme.borderStrong, true: theme.primary }}
                thumbColor={theme.text}
              />
            </View>

            <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 16, marginBottom: 8 }}>
              Notify me about
            </Text>
            <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 8 }}>
              {PREFS.map((p) => (
                <Chip
                  key={p.key}
                  label={p.label}
                  selected={(membership?.notification_pref ?? 'default') === p.key}
                  // Preferences are per membership row; without one there is
                  // nothing to PATCH and the server would answer 404.
                  onPress={() => {
                    if (membership && !saving) void savePrefs({ notification_pref: p.key })
                  }}
                  accessibilityLabel={`${p.label}: ${p.hint}`}
                />
              ))}
            </View>
            {!membership ? (
              <Text style={{ color: theme.warning, fontSize: 12, marginTop: 12 }}>
                You are not a member of this channel, so preferences do not apply.
              </Text>
            ) : null}
          </Section>

          <Section title="Channel">
            <NavRow
              label="Members"
              detail={memberCount === null ? undefined : String(memberCount)}
              onPress={() => navigation.navigate('Members', { channelId })}
            />
            <NavRow label="Pinned messages" onPress={() => navigation.navigate('Pins', { channelId })} />
            <NavRow
              label="Scheduled messages"
              onPress={() => navigation.navigate('Scheduled', { channelId })}
            />
          </Section>

          {!isDM ? (
            <Section title="Danger zone">
              <View
                style={{
                  flexDirection: tier === 'compact' ? 'column' : 'row',
                  gap: 12,
                }}
              >
                {membership?.role === 'admin' || workspaceIsAdmin ? (
                  <Button
                    label={channel?.is_archived ? 'Unarchive channel' : 'Archive channel'}
                    onPress={toggleArchive}
                    loading={busy}
                    variant="ghost"
                    inline
                  />
                ) : null}
                <Button label="Leave channel" onPress={leave} loading={busy} variant="danger" inline />
              </View>
            </Section>
          ) : null}
        </ScrollView>
      )}
    </SafeAreaView>
  )
}
