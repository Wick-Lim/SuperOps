import React, { useState, useEffect } from 'react'
import { View, Text, TextInput, Pressable, Alert } from 'react-native'
import { AuthLayout } from '../components/AuthLayout'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'
import { errorMessage } from '../api/client'
import { MIN_TOUCH } from './internal/ui'

type Props = NativeStackScreenProps<RootStackParamList, 'Invite'>

/** `POST /admin/invitations` answers `invite_url: "/invite/<token>"`. */
function extractToken(input: string): string {
  const trimmed = input.trim()
  const idx = trimmed.lastIndexOf('/invite/')
  return idx === -1 ? trimmed : trimmed.slice(idx + '/invite/'.length)
}

export default function InviteScreen({ navigation, route }: Props) {
  const [token, setToken] = useState(route.params?.token || '')
  const [inviteInfo, setInviteInfo] = useState<{
    email: string
    workspace_name: string
    inviter_name: string
  } | null>(null)
  const [form, setForm] = useState({ username: '', password: '', full_name: '' })
  const [loading, setLoading] = useState(false)

  useEffect(() => {
    const value = extractToken(token)
    if (value.length <= 10) {
      setInviteInfo(null)
      return
    }
    let cancelled = false
    authApi
      .getInviteInfo(value)
      .then((res) => {
        if (!cancelled) setInviteInfo(res.data)
      })
      .catch(() => {
        if (!cancelled) setInviteInfo(null)
      })
    return () => {
      cancelled = true
    }
  }, [token])

  const handleAccept = async () => {
    if (!form.username || !form.password) return
    setLoading(true)
    try {
      const res = await authApi.acceptInvite({
        token: extractToken(token),
        username: form.username,
        password: form.password,
        full_name: form.full_name,
      })
      // setTokens is async now — getMe would otherwise go out unauthenticated.
      await useAuthStore.getState().setTokens(res.data.access_token, res.data.refresh_token)
      const me = await authApi.getMe()
      await useAuthStore.getState().setUser(me.data)
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to accept invite'))
    } finally {
      setLoading(false)
    }
  }

  const inputStyle = {
    backgroundColor: '#0f172a',
    borderWidth: 1,
    borderColor: '#334155',
    borderRadius: 12,
    padding: 14,
    minHeight: MIN_TOUCH,
    color: '#fff',
    fontSize: 15,
    marginBottom: 12,
  } as const

  return (
    <AuthLayout>
        <View style={{ alignItems: 'center', marginBottom: 32 }}>
          <Text accessibilityRole="header" style={{ color: '#fff', fontSize: 24, fontWeight: 'bold' }}>
            Join SuperOps
          </Text>
          <Text style={{ color: '#94a3b8', marginTop: 4 }}>Paste your invite link or token</Text>
        </View>

        <TextInput
          value={token}
          onChangeText={setToken}
          placeholder="Invite link or token"
          placeholderTextColor="#94a3b8"
          autoCapitalize="none"
          autoCorrect={false}
          accessibilityLabel="Invite link or token"
          style={inputStyle}
        />

        {inviteInfo && (
          <View
            accessible
            accessibilityLabel={`Invited to ${inviteInfo.workspace_name} by ${inviteInfo.inviter_name}`}
            style={{
              backgroundColor: '#0f172a',
              borderRadius: 12,
              padding: 16,
              marginBottom: 16,
              borderWidth: 1,
              borderColor: '#334155',
            }}
          >
            <Text style={{ color: '#94a3b8', fontSize: 13 }}>You've been invited to</Text>
            <Text style={{ color: '#fff', fontSize: 18, fontWeight: '600', marginTop: 4 }}>
              {inviteInfo.workspace_name}
            </Text>
            <Text style={{ color: '#94a3b8', fontSize: 13, marginTop: 4 }}>
              by {inviteInfo.inviter_name} ({inviteInfo.email})
            </Text>
          </View>
        )}

        {inviteInfo && (
          <>
            <TextInput
              value={form.full_name}
              onChangeText={(v) => setForm((f) => ({ ...f, full_name: v }))}
              placeholder="Full name"
              placeholderTextColor="#94a3b8"
              accessibilityLabel="Full name"
              style={inputStyle}
            />
            <TextInput
              value={form.username}
              onChangeText={(v) => setForm((f) => ({ ...f, username: v }))}
              placeholder="Username"
              placeholderTextColor="#94a3b8"
              autoCapitalize="none"
              accessibilityLabel="Username"
              style={inputStyle}
            />
            <TextInput
              value={form.password}
              onChangeText={(v) => setForm((f) => ({ ...f, password: v }))}
              placeholder="Password (min 8 chars)"
              placeholderTextColor="#94a3b8"
              secureTextEntry
              accessibilityLabel="Password"
              accessibilityHint="At least eight characters"
              style={inputStyle}
            />

            <Pressable
              onPress={handleAccept}
              disabled={loading}
              accessibilityRole="button"
              accessibilityLabel="Join workspace"
              accessibilityState={{ disabled: loading, busy: loading }}
              style={{
                backgroundColor: '#4f46e5',
                borderRadius: 12,
                padding: 14,
                minHeight: MIN_TOUCH,
                alignItems: 'center',
                justifyContent: 'center',
                marginTop: 8,
                opacity: loading ? 0.6 : 1,
              }}
            >
              <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>
                {loading ? 'Joining...' : 'Join Workspace'}
              </Text>
            </Pressable>
          </>
        )}

        <Pressable
          onPress={() => navigation.navigate('Login')}
          accessibilityRole="button"
          accessibilityLabel="Sign in instead"
          style={{ marginTop: 24, minHeight: MIN_TOUCH, alignItems: 'center', justifyContent: 'center' }}
        >
          <Text style={{ color: '#94a3b8', fontSize: 14 }}>
            Already have an account? <Text style={{ color: '#818cf8' }}>Sign in</Text>
          </Text>
        </Pressable>
    </AuthLayout>
  )
}
