import React, { useState, useEffect } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, Alert } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'

type Props = NativeStackScreenProps<RootStackParamList, 'Invite'>

export default function InviteScreen({ navigation, route }: Props) {
  const [token, setToken] = useState(route.params?.token || '')
  const [inviteInfo, setInviteInfo] = useState<{ email: string; workspace_name: string; inviter_name: string } | null>(null)
  const [form, setForm] = useState({ username: '', password: '', full_name: '' })
  const [loading, setLoading] = useState(false)
  const { setTokens, setUser } = useAuthStore()

  useEffect(() => {
    if (token.length > 10) {
      authApi.getInviteInfo(token).then((res) => setInviteInfo(res.data)).catch(() => setInviteInfo(null))
    }
  }, [token])

  const handleAccept = async () => {
    if (!form.username || !form.password) return
    setLoading(true)
    try {
      const res = await authApi.acceptInvite({ token, username: form.username, password: form.password, full_name: form.full_name })
      setTokens(res.data.access_token, res.data.refresh_token)
      const me = await authApi.getMe()
      setUser(me.data)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to accept invite')
    } finally {
      setLoading(false)
    }
  }

  const inputStyle = { backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, marginBottom: 12 } as const

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <View style={{ flex: 1, justifyContent: 'center', paddingHorizontal: 32 }}>
        <View style={{ alignItems: 'center', marginBottom: 32 }}>
          <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold' }}>Join SuperOps</Text>
          <Text style={{ color: '#94a3b8', marginTop: 4 }}>Enter your invite token to get started</Text>
        </View>

        <TextInput value={token} onChangeText={setToken} placeholder="Invite token" placeholderTextColor="#64748b" autoCapitalize="none" style={inputStyle} />

        {inviteInfo && (
          <View style={{ backgroundColor: '#0f172a', borderRadius: 12, padding: 16, marginBottom: 16, borderWidth: 1, borderColor: '#334155' }}>
            <Text style={{ color: '#94a3b8', fontSize: 13 }}>You've been invited to</Text>
            <Text style={{ color: '#fff', fontSize: 18, fontWeight: '600', marginTop: 4 }}>{inviteInfo.workspace_name}</Text>
            <Text style={{ color: '#64748b', fontSize: 13, marginTop: 4 }}>by {inviteInfo.inviter_name} ({inviteInfo.email})</Text>
          </View>
        )}

        {inviteInfo && (
          <>
            <TextInput value={form.full_name} onChangeText={(v) => setForm((f) => ({ ...f, full_name: v }))} placeholder="Full Name" placeholderTextColor="#64748b" style={inputStyle} />
            <TextInput value={form.username} onChangeText={(v) => setForm((f) => ({ ...f, username: v }))} placeholder="Username" placeholderTextColor="#64748b" autoCapitalize="none" style={inputStyle} />
            <TextInput value={form.password} onChangeText={(v) => setForm((f) => ({ ...f, password: v }))} placeholder="Password (min 8 chars)" placeholderTextColor="#64748b" secureTextEntry style={inputStyle} />

            <Pressable onPress={handleAccept} disabled={loading}
              style={{ backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, alignItems: 'center', marginTop: 8, opacity: loading ? 0.6 : 1 }}>
              <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Joining...' : 'Join Workspace'}</Text>
            </Pressable>
          </>
        )}

        <Pressable onPress={() => navigation.navigate('Login')} style={{ marginTop: 24, alignItems: 'center' }}>
          <Text style={{ color: '#94a3b8', fontSize: 14 }}>
            Already have an account? <Text style={{ color: '#818cf8' }}>Sign in</Text>
          </Text>
        </Pressable>
      </View>
    </SafeAreaView>
  )
}
