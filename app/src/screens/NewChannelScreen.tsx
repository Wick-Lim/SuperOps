import React, { useEffect, useState } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, ScrollView, Switch, ActivityIndicator, Alert } from 'react-native'
import { theme } from '../lib/theme'
import type { Channel } from '../lib/types'
import { channelApi } from '../api/channels'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { errorMessage } from '../api/client'
import { MIN_TOUCH, ScreenHeader } from './internal/ui'

function slugify(name: string): string {
  return name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9\s-]/g, '')
    .replace(/\s+/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-+|-+$/g, '')
}

export default function NewChannelScreen({ navigation }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const addChannel = useChannelStore((s) => s.addChannel)
  const channels = useChannelStore((s) => s.channels)

  const [name, setName] = useState('')
  const [description, setDescription] = useState('')
  const [isPrivate, setIsPrivate] = useState(false)
  const [creating, setCreating] = useState(false)

  const [browseable, setBrowseable] = useState<Channel[]>([])
  const [browseLoading, setBrowseLoading] = useState(true)
  const [browseError, setBrowseError] = useState<string | null>(null)
  const [joiningId, setJoiningId] = useState<string | null>(null)

  const slug = slugify(name)
  const joinedIds = new Set(channels.map((c) => c.id))

  const loadBrowse = () => {
    if (!activeWorkspace) return
    setBrowseLoading(true)
    setBrowseError(null)
    channelApi
      .browse(activeWorkspace.id)
      .then((res) => setBrowseable(res.data ?? []))
      .catch((err) => setBrowseError(errorMessage(err, 'Failed to load channels')))
      .finally(() => setBrowseLoading(false))
  }

  useEffect(() => {
    loadBrowse()
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [activeWorkspace?.id])

  const handleCreate = async () => {
    if (!activeWorkspace) {
      Alert.alert('Error', 'No active workspace')
      return
    }
    if (!name.trim() || !slug) {
      Alert.alert('Error', 'Channel name is required')
      return
    }
    setCreating(true)
    try {
      const res = await channelApi.create(activeWorkspace.id, {
        name: name.trim(),
        slug,
        description: description.trim() || undefined,
        type: isPrivate ? 'private' : 'public',
      })
      addChannel(res.data)
      navigation.goBack()
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to create channel'))
    } finally {
      setCreating(false)
    }
  }

  const handleJoin = async (ch: Channel) => {
    if (!activeWorkspace) return
    setJoiningId(ch.id)
    try {
      await channelApi.join(activeWorkspace.id, ch.id)
      addChannel(ch)
      setBrowseable((prev) => prev.filter((c) => c.id !== ch.id))
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to join channel'))
    } finally {
      setJoiningId(null)
    }
  }

  const browseList = browseable.filter((c) => !joinedIds.has(c.id))

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader title="New channel" backLabel="Cancel" onBack={() => navigation.goBack()} />

      <ScrollView contentContainerStyle={{ padding: 16, paddingBottom: 40 }} keyboardShouldPersistTaps="handled">
        {/* Create form */}
        <Text style={labelStyle}>NAME</Text>
        <TextInput
          value={name}
          onChangeText={setName}
          placeholder="e.g. marketing"
          placeholderTextColor={theme.textMuted}
          accessibilityLabel="Channel name"
          accessibilityHint="Lowercase letters, numbers and dashes become the channel URL"

          autoCapitalize="none"
          autoCorrect={false}
          style={inputStyle}
        />
        {slug ? (
          <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 6 }}>
            URL: <Text style={{ color: theme.textMuted }}>#{slug}</Text>
          </Text>
        ) : null}

        <Text style={[labelStyle, { marginTop: 18 }]}>DESCRIPTION</Text>
        <TextInput
          value={description}
          onChangeText={setDescription}
          placeholder="What's this channel about?"
          placeholderTextColor={theme.textMuted}
          accessibilityLabel="Channel description"

          multiline
          style={[inputStyle, { minHeight: 72, textAlignVertical: 'top' }]}
        />

        <View
          style={{
            flexDirection: 'row',
            alignItems: 'center',
            justifyContent: 'space-between',
            marginTop: 18,
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
            borderRadius: 12,
            padding: 14,
          }}
        >
          <View style={{ flex: 1, paddingRight: 12 }}>
            <Text style={{ color: theme.text, fontSize: 15, fontWeight: '500' }}>Private channel</Text>
            <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 2 }}>
              Only invited members can view and join
            </Text>
          </View>
          <Switch
            value={isPrivate}
            onValueChange={setIsPrivate}
            accessibilityLabel="Private channel"
            accessibilityHint="Only invited members can view and join"

            trackColor={{ false: theme.borderStrong, true: theme.primary }}
            thumbColor={theme.text}
          />
        </View>

        <Pressable
          onPress={handleCreate}
          disabled={creating || !name.trim()}
          accessibilityRole="button"
          accessibilityLabel="Create channel"
          accessibilityState={{ disabled: creating || !name.trim(), busy: creating }}
          style={{
            backgroundColor: theme.primary,
            borderRadius: 12,
            padding: 14,
            minHeight: MIN_TOUCH,
            justifyContent: 'center',
            alignItems: 'center',
            marginTop: 20,
            opacity: creating || !name.trim() ? 0.5 : 1,
          }}
        >
          <Text style={{ color: theme.primaryText, fontSize: 15, fontWeight: '600' }}>
            {creating ? 'Creating...' : 'Create Channel'}
          </Text>
        </Pressable>

        {/* Browse public channels */}
        <View style={{ flexDirection: 'row', alignItems: 'center', marginTop: 32, marginBottom: 4 }}>
          <Text style={[labelStyle, { flex: 1 }]}>BROWSE PUBLIC CHANNELS</Text>
          <Pressable
            onPress={loadBrowse}
            accessibilityRole="button"
            accessibilityLabel="Refresh the list of public channels"
            hitSlop={16}
            style={{ minHeight: 32, justifyContent: 'center' }}
          >
            <Text style={{ color: theme.accent, fontSize: 12 }}>Refresh</Text>
          </Pressable>
        </View>

        {browseLoading ? (
          <View style={{ paddingVertical: 24, alignItems: 'center' }}>
            <ActivityIndicator color={theme.primary} />
          </View>
        ) : browseError ? (
          <View style={{ paddingVertical: 16 }}>
            <Text style={{ color: theme.danger, fontSize: 14 }}>{browseError}</Text>
            <Pressable onPress={loadBrowse} style={{ marginTop: 8 }}>
              <Text style={{ color: theme.accent, fontSize: 14 }}>Try again</Text>
            </Pressable>
          </View>
        ) : browseList.length === 0 ? (
          <Text style={{ color: theme.textMuted, fontSize: 14, paddingVertical: 12 }}>
            No public channels available to join.
          </Text>
        ) : (
          browseList.map((ch) => (
            <View
              key={ch.id}
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                paddingVertical: 12,
                borderBottomWidth: 1,
                borderBottomColor: theme.border,
              }}
            >
              <Text style={{ color: theme.textMuted, fontSize: 16, marginRight: 8 }}>#</Text>
              <View style={{ flex: 1 }}>
                <Text style={{ color: theme.body, fontSize: 15, fontWeight: '500' }}>
                  {ch.name || ch.slug || 'unnamed'}
                </Text>
                {ch.description ? (
                  <Text style={{ color: theme.textMuted, fontSize: 12, marginTop: 2 }} numberOfLines={1}>
                    {ch.description}
                  </Text>
                ) : null}
              </View>
              <Pressable
                onPress={() => handleJoin(ch)}
                disabled={joiningId === ch.id}
                accessibilityRole="button"
                accessibilityLabel={`Join ${ch.name || ch.slug || 'channel'}`}
                accessibilityState={{ disabled: joiningId === ch.id, busy: joiningId === ch.id }}
                style={{
                  paddingHorizontal: 16,
                  minHeight: MIN_TOUCH,
                  justifyContent: 'center',
                  borderRadius: 8,
                  borderWidth: 1,
                  borderColor: theme.primary,
                  opacity: joiningId === ch.id ? 0.5 : 1,
                }}
              >
                <Text style={{ color: theme.accent, fontSize: 13, fontWeight: '600' }}>
                  {joiningId === ch.id ? 'Joining...' : 'Join'}
                </Text>
              </Pressable>
            </View>
          ))
        )}
      </ScrollView>
    </SafeAreaView>
  )
}

const labelStyle = {
  color: theme.textMuted,
  fontSize: 11,
  fontWeight: '700' as const,
  letterSpacing: 1,
}

const inputStyle = {
  backgroundColor: theme.surface,
  borderWidth: 1,
  borderColor: theme.borderStrong,
  borderRadius: 12,
  padding: 14,
  color: theme.text,
  fontSize: 15,
  marginTop: 6,
}
