import React, { useState } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, Alert } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'

type Props = NativeStackScreenProps<RootStackParamList, 'Login'>

export default function LoginScreen({ navigation }: Props) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [loading, setLoading] = useState(false)
  const { setTokens, setUser } = useAuthStore()

  const handleLogin = async () => {
    if (!email || !password) return
    setLoading(true)
    try {
      const res = await authApi.login({ email, password })
      setTokens(res.data.access_token, res.data.refresh_token)
      const me = await authApi.getMe()
      setUser(me.data)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Login failed')
    } finally {
      setLoading(false)
    }
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <View style={{ flex: 1, justifyContent: 'center', paddingHorizontal: 32 }}>
        <View style={{ alignItems: 'center', marginBottom: 40 }}>
          <View style={{ width: 48, height: 48, backgroundColor: '#4f46e5', borderRadius: 12, alignItems: 'center', justifyContent: 'center', marginBottom: 16 }}>
            <Text style={{ color: '#fff', fontSize: 20, fontWeight: 'bold' }}>S</Text>
          </View>
          <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold' }}>SuperOps</Text>
          <Text style={{ color: '#94a3b8', marginTop: 4 }}>Sign in to your workspace</Text>
        </View>

        <TextInput
          value={email} onChangeText={setEmail}
          placeholder="Email" placeholderTextColor="#64748b"
          keyboardType="email-address" autoCapitalize="none"
          style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, marginBottom: 12 }}
        />
        <TextInput
          value={password} onChangeText={setPassword}
          placeholder="Password" placeholderTextColor="#64748b"
          secureTextEntry
          style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, marginBottom: 20 }}
        />

        <Pressable onPress={handleLogin} disabled={loading}
          style={{ backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, alignItems: 'center', opacity: loading ? 0.6 : 1 }}>
          <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Signing in...' : 'Sign In'}</Text>
        </Pressable>

        <Pressable onPress={() => navigation.navigate('Invite', {})} style={{ marginTop: 24, alignItems: 'center' }}>
          <Text style={{ color: '#94a3b8', fontSize: 14 }}>
            Have an invite? <Text style={{ color: '#818cf8' }}>Join workspace</Text>
          </Text>
        </Pressable>
      </View>
    </SafeAreaView>
  )
}
