import React, { useEffect, useState } from 'react'
import { View, Text, TextInput, Pressable, FlatList, SafeAreaView, ActivityIndicator, Alert } from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import { searchApi, type SearchHit } from '../api/search'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useUserStore, displayName } from '../stores/userStore'

export default function SearchScreen({ navigation }: { navigation: any; route: any }) {
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const channels = useChannelStore((s) => s.channels)
  const setActiveChannel = useChannelStore((s) => s.setActiveChannel)
  const users = useUserStore((s) => s.users)
  const ensureUsers = useUserStore((s) => s.ensureUsers)

  const [query, setQuery] = useState('')
  const [hits, setHits] = useState<SearchHit[]>([])
  const [total, setTotal] = useState(0)
  const [loading, setLoading] = useState(false)
  const [searched, setSearched] = useState(false)

  useEffect(() => {
    if (hits.length > 0) ensureUsers(hits.map((h) => h.user_id))
  }, [hits])

  const runSearch = async () => {
    const q = query.trim()
    if (!q || !activeWorkspace) return
    setLoading(true)
    setSearched(true)
    try {
      const res = await searchApi.messages(activeWorkspace.id, q)
      setHits(res.data?.hits ?? [])
      setTotal(res.data?.estimated_total ?? 0)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Search failed')
      setHits([])
      setTotal(0)
    } finally {
      setLoading(false)
    }
  }

  const channelName = (channelId: string) => {
    const ch = channels.find((c) => c.id === channelId)
    return ch?.name || 'channel'
  }

  const openHit = (hit: SearchHit) => {
    const ch = channels.find((c) => c.id === hit.channel_id)
    if (ch) {
      setActiveChannel(ch)
      navigation.navigate('Workspace', {})
    } else {
      Alert.alert('Unavailable', 'This message is in a channel you are not a member of.')
    }
  }

  const renderHit = ({ item }: { item: SearchHit }) => (
    <Pressable
      onPress={() => openHit(item)}
      style={{ flexDirection: 'row', gap: 10, paddingHorizontal: 16, paddingVertical: 12, borderBottomWidth: 1, borderBottomColor: theme.border }}
    >
      <View style={{ width: 36, height: 36, borderRadius: 10, backgroundColor: avatarColor(item.user_id), alignItems: 'center', justifyContent: 'center' }}>
        <Text style={{ color: '#fff', fontSize: 12, fontWeight: '600' }}>{item.user_id.slice(0, 2).toUpperCase()}</Text>
      </View>
      <View style={{ flex: 1 }}>
        <View style={{ flexDirection: 'row', alignItems: 'baseline', gap: 8, marginBottom: 2 }}>
          <Text style={{ color: theme.text, fontSize: 14, fontWeight: '600' }}>{displayName(users, item.user_id)}</Text>
          <Text style={{ color: theme.textFaint, fontSize: 12 }}>#{channelName(item.channel_id)}</Text>
        </View>
        <Text style={{ color: theme.body, fontSize: 15, lineHeight: 21 }} numberOfLines={3}>{item.content}</Text>
      </View>
    </Pressable>
  )

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      {/* Header */}
      <View style={{ height: 56, paddingHorizontal: 12, flexDirection: 'row', alignItems: 'center', borderBottomWidth: 1, borderBottomColor: theme.border }}>
        <Pressable onPress={() => navigation.goBack()} hitSlop={8} style={{ paddingHorizontal: 8, paddingVertical: 6 }}>
          <Text style={{ color: theme.accent, fontSize: 15 }}>Back</Text>
        </Pressable>
        <Text style={{ color: theme.text, fontSize: 16, fontWeight: '600', flex: 1, marginLeft: 4 }}>Search</Text>
      </View>

      {/* Search box */}
      <View style={{ paddingHorizontal: 16, paddingTop: 12, paddingBottom: 4 }}>
        <TextInput
          value={query}
          onChangeText={setQuery}
          onSubmitEditing={runSearch}
          returnKeyType="search"
          autoFocus
          placeholder="Search messages..."
          placeholderTextColor={theme.textFaint}
          style={{ backgroundColor: theme.surface, borderWidth: 1, borderColor: theme.borderStrong, borderRadius: 12, paddingHorizontal: 14, paddingVertical: 12, color: theme.text, fontSize: 15 }}
        />
      </View>

      {searched && !loading && (
        <Text style={{ color: theme.textFaint, fontSize: 12, paddingHorizontal: 16, paddingVertical: 8 }}>
          {total} {total === 1 ? 'result' : 'results'}
        </Text>
      )}

      {loading ? (
        <View style={{ flex: 1, alignItems: 'center', justifyContent: 'center' }}>
          <ActivityIndicator color={theme.accent} />
        </View>
      ) : (
        <FlatList
          data={hits}
          keyExtractor={(item) => item.id}
          renderItem={renderHit}
          keyboardShouldPersistTaps="handled"
          ListEmptyComponent={
            <View style={{ alignItems: 'center', justifyContent: 'center', paddingTop: 64, paddingHorizontal: 32 }}>
              <Text style={{ color: theme.textFaint, fontSize: 15, textAlign: 'center' }}>
                {searched ? 'No messages found.' : 'Search across your workspace messages.'}
              </Text>
            </View>
          }
        />
      )}
    </SafeAreaView>
  )
}
