import React, { useState, useEffect } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, Alert } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { workspaceApi } from '../api/workspaces'
import { useWorkspaceStore } from '../stores/workspaceStore'

type Props = NativeStackScreenProps<RootStackParamList, 'Setup'>

export default function SetupScreen({ navigation }: Props) {
  const [name, setName] = useState('')
  const [loading, setLoading] = useState(false)
  const { setWorkspaces, setActiveWorkspace } = useWorkspaceStore()

  useEffect(() => {
    workspaceApi.list().then((res) => {
      if (res.data && res.data.length > 0) {
        setWorkspaces(res.data)
        setActiveWorkspace(res.data[0])
        navigation.replace('Workspace', { workspaceId: res.data[0].id })
      }
    }).catch(() => {})
  }, [])

  const handleCreate = async () => {
    if (!name.trim()) return
    setLoading(true)
    const slug = name.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
    try {
      const res = await workspaceApi.create({ name, slug })
      setWorkspaces([res.data])
      setActiveWorkspace(res.data)
      navigation.replace('Workspace', { workspaceId: res.data.id })
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to create workspace')
    } finally {
      setLoading(false)
    }
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <View style={{ flex: 1, justifyContent: 'center', paddingHorizontal: 32 }}>
        <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold', textAlign: 'center' }}>Create a Workspace</Text>
        <Text style={{ color: '#94a3b8', textAlign: 'center', marginTop: 4, marginBottom: 32 }}>Set up your team's workspace</Text>

        <TextInput
          value={name} onChangeText={setName}
          placeholder="Workspace name" placeholderTextColor="#64748b"
          style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, marginBottom: 20 }}
        />

        <Pressable onPress={handleCreate} disabled={loading}
          style={{ backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, alignItems: 'center', opacity: loading ? 0.6 : 1 }}>
          <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Creating...' : 'Create Workspace'}</Text>
        </Pressable>
      </View>
    </SafeAreaView>
  )
}
