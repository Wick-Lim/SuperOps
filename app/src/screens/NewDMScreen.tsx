import React, { useEffect, useRef, useState } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, FlatList, ActivityIndicator, Alert } from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import type { PublicUser } from '../lib/types'
import { userApi } from '../api/users'
import { channelApi } from '../api/channels'
import { useAuthStore } from '../stores/authStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useUserStore } from '../stores/userStore'

function initial(u: PublicUser): string {
  return (u.full_name || u.username || '?').trim().charAt(0).toUpperCase() || '?'
}

export default function NewDMScreen({ navigation }: { navigation: any; route: any }) {
  const me = useAuthStore((s) => s.user)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const addChannel = useChannelStore((s) => s.addChannel)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const setUser = useUserStore((s) => s.setUser)

  const [query, setQuery] = useState('')
  const [results, setResults] = useState<PublicUser[]>([])
  const [searching, setSearching] = useState(false)
  const [searchError, setSearchError] = useState<string | null>(null)
  const [selected, setSelected] = useState<PublicUser[]>([])
  const [starting, setStarting] = useState(false)

  const debounce = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    if (debounce.current) clearTimeout(debounce.current)
    const q = query.trim()
    if (!q) {
      setResults([])
      setSearching(false)
      setSearchError(null)
      return
    }
    setSearching(true)
    debounce.current = setTimeout(() => {
      userApi
        .search(q)
        .then((res) => {
          setSearchError(null)
          setResults((res.data ?? []).filter((u) => u.id !== me?.id))
        })
        .catch((err) => setSearchError(err instanceof Error ? err.message : 'Search failed'))
        .finally(() => setSearching(false))
    }, 300)
    return () => {
      if (debounce.current) clearTimeout(debounce.current)
    }
  }, [query, me?.id])

  const isSelected = (id: string) => selected.some((u) => u.id === id)

  const toggle = (u: PublicUser) => {
    setSelected((prev) => (prev.some((s) => s.id === u.id) ? prev.filter((s) => s.id !== u.id) : [...prev, u]))
  }

  const removeSelected = (id: string) => setSelected((prev) => prev.filter((u) => u.id !== id))

  const handleStart = async () => {
    if (!activeWorkspace) {
      Alert.alert('Error', 'No active workspace')
      return
    }
    if (selected.length === 0) return
    setStarting(true)
    try {
      const res = await channelApi.createDM(
        activeWorkspace.id,
        selected.map((u) => u.id),
      )
      // Cache the participant profiles so the conversation renders names immediately.
      selected.forEach((u) => setUser(u))
      addChannel(res.data)
      setActiveChannel(res.data)
      navigation.navigate('Workspace', { workspaceId: activeWorkspace.id })
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to start conversation')
    } finally {
      setStarting(false)
    }
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
          <Text style={{ color: theme.accent, fontSize: 16 }}>Cancel</Text>
        </Pressable>
        <Text style={{ color: theme.text, fontSize: 17, fontWeight: '600', flex: 1 }}>New Message</Text>
        <Pressable onPress={handleStart} disabled={selected.length === 0 || starting} hitSlop={8}>
          <Text
            style={{
              color: selected.length === 0 || starting ? theme.textFaint : theme.accent,
              fontSize: 15,
              fontWeight: '600',
            }}
          >
            {starting ? '...' : 'Start'}
          </Text>
        </Pressable>
      </View>

      {/* Selected chips */}
      {selected.length > 0 ? (
        <View
          style={{
            flexDirection: 'row',
            flexWrap: 'wrap',
            paddingHorizontal: 12,
            paddingTop: 12,
            gap: 8,
          }}
        >
          {selected.map((u) => (
            <Pressable
              key={u.id}
              onPress={() => removeSelected(u.id)}
              style={{
                flexDirection: 'row',
                alignItems: 'center',
                backgroundColor: theme.surfaceAlt,
                borderRadius: 16,
                paddingLeft: 8,
                paddingRight: 10,
                paddingVertical: 6,
              }}
            >
              <View
                style={{
                  width: 20,
                  height: 20,
                  borderRadius: 10,
                  backgroundColor: avatarColor(u.id),
                  alignItems: 'center',
                  justifyContent: 'center',
                  marginRight: 6,
                }}
              >
                <Text style={{ color: theme.text, fontSize: 10, fontWeight: '700' }}>{initial(u)}</Text>
              </View>
              <Text style={{ color: theme.body, fontSize: 13, fontWeight: '500' }}>
                {u.full_name || u.username}
              </Text>
              <Text style={{ color: theme.textFaint, fontSize: 14, marginLeft: 6 }}>×</Text>
            </Pressable>
          ))}
        </View>
      ) : null}

      {/* Search input */}
      <View style={{ paddingHorizontal: 16, paddingTop: 12, paddingBottom: 8 }}>
        <TextInput
          value={query}
          onChangeText={setQuery}
          placeholder="Search people by name or username"
          placeholderTextColor={theme.textFaint}
          autoCapitalize="none"
          autoCorrect={false}
          style={{
            backgroundColor: theme.surface,
            borderWidth: 1,
            borderColor: theme.borderStrong,
            borderRadius: 12,
            padding: 14,
            color: theme.text,
            fontSize: 15,
          }}
        />
      </View>

      {/* Results */}
      {searching ? (
        <View style={{ paddingVertical: 24, alignItems: 'center' }}>
          <ActivityIndicator color={theme.primary} />
        </View>
      ) : searchError ? (
        <View style={{ padding: 16 }}>
          <Text style={{ color: theme.danger, fontSize: 14 }}>{searchError}</Text>
        </View>
      ) : (
        <FlatList
          data={results}
          keyExtractor={(u) => u.id}
          keyboardShouldPersistTaps="handled"
          ListEmptyComponent={
            query.trim() ? (
              <Text style={{ color: theme.textFaint, fontSize: 14, textAlign: 'center', paddingTop: 32 }}>
                No people found.
              </Text>
            ) : (
              <Text style={{ color: theme.textFaint, fontSize: 14, textAlign: 'center', paddingTop: 32 }}>
                Search for people to start a conversation.
              </Text>
            )
          }
          renderItem={({ item: u }) => {
            const sel = isSelected(u.id)
            return (
              <Pressable
                onPress={() => toggle(u)}
                style={{
                  flexDirection: 'row',
                  alignItems: 'center',
                  paddingHorizontal: 16,
                  paddingVertical: 12,
                  borderBottomWidth: 1,
                  borderBottomColor: theme.border,
                }}
              >
                <View
                  style={{
                    width: 36,
                    height: 36,
                    borderRadius: 18,
                    backgroundColor: avatarColor(u.id),
                    alignItems: 'center',
                    justifyContent: 'center',
                    marginRight: 12,
                  }}
                >
                  <Text style={{ color: theme.text, fontWeight: '700', fontSize: 14 }}>{initial(u)}</Text>
                </View>
                <View style={{ flex: 1 }}>
                  <Text style={{ color: theme.body, fontSize: 15, fontWeight: '500' }}>
                    {u.full_name || u.username}
                  </Text>
                  <Text style={{ color: theme.textFaint, fontSize: 12, marginTop: 1 }}>
                    @{u.username}
                    {u.is_bot ? '  ·  bot' : ''}
                  </Text>
                </View>
                <View
                  style={{
                    width: 22,
                    height: 22,
                    borderRadius: 11,
                    borderWidth: 2,
                    borderColor: sel ? theme.primary : theme.borderStrong,
                    backgroundColor: sel ? theme.primary : 'transparent',
                    alignItems: 'center',
                    justifyContent: 'center',
                  }}
                >
                  {sel ? <Text style={{ color: theme.primaryText, fontSize: 12, fontWeight: '700' }}>✓</Text> : null}
                </View>
              </Pressable>
            )
          }}
        />
      )}
    </SafeAreaView>
  )
}
