import React, { useState } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, Alert } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'

type Props = NativeStackScreenProps<RootStackParamList, 'Register'>

export default function RegisterScreen({ navigation }: Props) {
  const [form, setForm] = useState({ full_name: '', username: '', email: '', password: '' })
  const [loading, setLoading] = useState(false)
  const { setTokens, setUser } = useAuthStore()

  const update = (field: string, value: string) => setForm((f) => ({ ...f, [field]: value }))

  const handleRegister = async () => {
    if (!form.email || !form.username || !form.password) return
    setLoading(true)
    try {
      await authApi.register(form)
      const loginRes = await authApi.login({ email: form.email, password: form.password })
      setTokens(loginRes.data.access_token, loginRes.data.refresh_token)
      const me = await authApi.getMe()
      setUser(me.data)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Registration failed')
    } finally {
      setLoading(false)
    }
  }

  const inputStyle = { backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, marginBottom: 12 } as const

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <View style={{ flex: 1, justifyContent: 'center', paddingHorizontal: 32 }}>
        <View style={{ alignItems: 'center', marginBottom: 32 }}>
          <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold' }}>Create Account</Text>
          <Text style={{ color: '#94a3b8', marginTop: 4 }}>Get started with SuperOps</Text>
        </View>

        <TextInput value={form.full_name} onChangeText={(v) => update('full_name', v)} placeholder="Full Name" placeholderTextColor="#64748b" style={inputStyle} />
        <TextInput value={form.username} onChangeText={(v) => update('username', v)} placeholder="Username" placeholderTextColor="#64748b" autoCapitalize="none" style={inputStyle} />
        <TextInput value={form.email} onChangeText={(v) => update('email', v)} placeholder="Email" placeholderTextColor="#64748b" keyboardType="email-address" autoCapitalize="none" style={inputStyle} />
        <TextInput value={form.password} onChangeText={(v) => update('password', v)} placeholder="Password (min 8 chars)" placeholderTextColor="#64748b" secureTextEntry style={inputStyle} />

        <Pressable onPress={handleRegister} disabled={loading}
          style={{ backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, alignItems: 'center', marginTop: 8, opacity: loading ? 0.6 : 1 }}>
          <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Creating...' : 'Create Account'}</Text>
        </Pressable>

        <Pressable onPress={() => navigation.navigate('Login')} style={{ marginTop: 24, alignItems: 'center' }}>
          <Text style={{ color: '#94a3b8', fontSize: 14 }}>
            Already have an account? <Text style={{ color: '#818cf8' }}>Sign in</Text>
          </Text>
        </Pressable>
      </View>
    </SafeAreaView>
  )
}
