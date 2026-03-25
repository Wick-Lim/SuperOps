import React, { useEffect, useState } from 'react'
import { View, Text, FlatList, Pressable, SafeAreaView } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { channelApi } from '../api/channels'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useChannelStore } from '../stores/channelStore'
import { useAuthStore } from '../stores/authStore'
import { wsManager } from '../lib/websocket'
import ChannelView from '../components/channel/ChannelView'

type Props = NativeStackScreenProps<RootStackParamList, 'Workspace'>

export default function WorkspaceScreen({ route }: Props) {
  const { workspaceId } = route.params
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const { channels, setChannels, setActiveChannel } = useChannelStore()
  const activeChannel = useChannelStore((s) => s.activeChannel)
  const user = useAuthStore((s) => s.user)
  const logout = useAuthStore((s) => s.logout)
  const [sidebarVisible, setSidebarVisible] = useState(true)

  useEffect(() => {
    channelApi.list(workspaceId).then((res) => setChannels(res.data)).catch(() => {})
    wsManager.connect()
    return () => wsManager.disconnect()
  }, [workspaceId])

  useEffect(() => {
    channels.forEach((ch) => wsManager.subscribe(ch.id))
    return () => channels.forEach((ch) => wsManager.unsubscribe(ch.id))
  }, [channels])

  const selectChannel = (ch: typeof channels[0]) => {
    setActiveChannel(ch)
    setSidebarVisible(false)
  }

  // Sidebar view
  if (sidebarVisible && !activeChannel) {
    return (
      <SafeAreaView style={{ flex: 1, backgroundColor: '#0f172a' }}>
        {/* Header */}
        <View style={{ height: 56, paddingHorizontal: 16, flexDirection: 'row', alignItems: 'center', borderBottomWidth: 1, borderBottomColor: '#1e293b' }}>
          <View style={{ width: 32, height: 32, backgroundColor: '#4f46e5', borderRadius: 8, alignItems: 'center', justifyContent: 'center', marginRight: 12 }}>
            <Text style={{ color: '#fff', fontWeight: 'bold', fontSize: 14 }}>{workspace?.name?.[0] || 'S'}</Text>
          </View>
          <Text style={{ color: '#fff', fontWeight: '600', fontSize: 16, flex: 1 }}>{workspace?.name || 'SuperOps'}</Text>
          <Pressable onPress={logout}>
            <Text style={{ color: '#94a3b8', fontSize: 12 }}>Logout</Text>
          </Pressable>
        </View>

        {/* Channel list */}
        <Text style={{ color: '#94a3b8', fontSize: 11, fontWeight: '700', paddingHorizontal: 16, paddingTop: 16, paddingBottom: 8, letterSpacing: 1 }}>CHANNELS</Text>
        <FlatList
          data={channels}
          keyExtractor={(ch) => ch.id}
          renderItem={({ item: ch }) => (
            <Pressable onPress={() => selectChannel(ch)}
              style={{ paddingHorizontal: 16, paddingVertical: 10, flexDirection: 'row', alignItems: 'center' }}>
              <Text style={{ color: '#64748b', marginRight: 8 }}>#</Text>
              <Text style={{ color: '#e2e8f0', fontSize: 15 }}>{ch.name || 'unnamed'}</Text>
            </Pressable>
          )}
        />

        {/* User footer */}
        <View style={{ height: 56, paddingHorizontal: 16, flexDirection: 'row', alignItems: 'center', borderTopWidth: 1, borderTopColor: '#1e293b' }}>
          <View style={{ width: 32, height: 32, backgroundColor: '#16a34a', borderRadius: 16, alignItems: 'center', justifyContent: 'center', marginRight: 8 }}>
            <Text style={{ color: '#fff', fontWeight: '600', fontSize: 12 }}>{user?.full_name?.[0] || '?'}</Text>
          </View>
          <Text style={{ color: '#fff', fontSize: 14, fontWeight: '500' }}>{user?.full_name || user?.username}</Text>
        </View>
      </SafeAreaView>
    )
  }

  // Channel view
  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      {activeChannel ? (
        <ChannelView channel={activeChannel} onBack={() => { setActiveChannel(null); setSidebarVisible(true) }} />
      ) : (
        <View style={{ flex: 1, justifyContent: 'center', alignItems: 'center' }}>
          <Text style={{ color: '#64748b', fontSize: 16 }}>Select a channel</Text>
        </View>
      )}
    </SafeAreaView>
  )
}
