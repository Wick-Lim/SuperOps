import React, { useState } from 'react'
import { View, Text, TextInput, Pressable, Alert } from 'react-native'
import { AuthLayout } from '../components/AuthLayout'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'
import { ServerErrorCode, errorMessage, hasErrorCode } from '../api/client'
import { MIN_TOUCH } from './internal/ui'

type Props = NativeStackScreenProps<RootStackParamList, 'Login'>

export default function LoginScreen({ navigation }: Props) {
  const [email, setEmail] = useState('')
  const [password, setPassword] = useState('')
  const [totpCode, setTotpCode] = useState('')
  const [requires2fa, setRequires2fa] = useState(false)
  const [loading, setLoading] = useState(false)

  const handleLogin = async () => {
    if (!email || !password) return
    if (requires2fa && !totpCode) return
    setLoading(true)
    try {
      const res = await authApi.login({
        email,
        password,
        totp_code: requires2fa ? totpCode : undefined,
      })
      // setTokens must land before getMe, or the request goes out unauthenticated.
      await useAuthStore.getState().setTokens(res.data.access_token, res.data.refresh_token)
      const me = await authApi.getMe()
      await useAuthStore.getState().setUser(me.data)
    } catch (err) {
      // Branch on the typed code. The old test was /two-factor/i against the
      // error message, which the server never sends — the field was unreachable.
      if (hasErrorCode(err, ServerErrorCode.TotpRequired)) {
        setRequires2fa(true)
        setTotpCode('')
        Alert.alert('Two-factor required', 'Enter the 6-digit code from your authenticator app.')
      } else if (hasErrorCode(err, ServerErrorCode.InvalidTotpCode)) {
        setTotpCode('')
        Alert.alert('Incorrect code', 'That code was not accepted. Codes expire every 30 seconds.')
      } else {
        Alert.alert('Error', errorMessage(err, 'Login failed'))
      }
    } finally {
      setLoading(false)
    }
  }

  return (
    <AuthLayout>
        <View style={{ alignItems: 'center', marginBottom: 40 }}>
          <View style={{ width: 48, height: 48, backgroundColor: '#4f46e5', borderRadius: 12, alignItems: 'center', justifyContent: 'center', marginBottom: 16 }}>
            <Text style={{ color: '#fff', fontSize: 20, fontWeight: 'bold' }}>S</Text>
          </View>
          <Text accessibilityRole="header" style={{ color: '#fff', fontSize: 24, fontWeight: 'bold' }}>SuperOps</Text>
          <Text style={{ color: '#94a3b8', marginTop: 4 }}>Sign in to your workspace</Text>
        </View>

        <TextInput
          value={email} onChangeText={setEmail}
          placeholder="Email" placeholderTextColor="#94a3b8"
          keyboardType="email-address" autoCapitalize="none" autoComplete="email"
          accessibilityLabel="Email"
          style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, minHeight: MIN_TOUCH, color: '#fff', fontSize: 15, marginBottom: 12 }}
        />
        <TextInput
          value={password} onChangeText={setPassword}
          placeholder="Password" placeholderTextColor="#94a3b8"
          secureTextEntry autoComplete="password"
          accessibilityLabel="Password"
          style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, minHeight: MIN_TOUCH, color: '#fff', fontSize: 15, marginBottom: requires2fa ? 12 : 20 }}
        />

        {requires2fa && (
          <TextInput
            value={totpCode} onChangeText={setTotpCode}
            placeholder="6-digit code" placeholderTextColor="#94a3b8"
            keyboardType="number-pad" maxLength={6} autoFocus autoComplete="one-time-code"
            accessibilityLabel="Two-factor authentication code"
            accessibilityHint="Six digits from your authenticator app"
            style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, minHeight: MIN_TOUCH, color: '#fff', fontSize: 15, marginBottom: 20, letterSpacing: 4, textAlign: 'center' }}
          />
        )}

        <Pressable
          onPress={handleLogin}
          disabled={loading}
          accessibilityRole="button"
          accessibilityLabel="Sign in"
          accessibilityState={{ disabled: loading, busy: loading }}
          style={{ backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, minHeight: MIN_TOUCH, justifyContent: 'center', alignItems: 'center', opacity: loading ? 0.6 : 1 }}
        >
          <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Signing in...' : 'Sign In'}</Text>
        </Pressable>

        <Pressable
          onPress={() => navigation.navigate('Invite', {})}
          accessibilityRole="button"
          accessibilityLabel="Join a workspace with an invite"
          style={{ marginTop: 24, minHeight: MIN_TOUCH, justifyContent: 'center', alignItems: 'center' }}
        >
          <Text style={{ color: '#94a3b8', fontSize: 14 }}>
            Have an invite? <Text style={{ color: '#818cf8' }}>Join workspace</Text>
          </Text>
        </Pressable>
    </AuthLayout>
  )
}
