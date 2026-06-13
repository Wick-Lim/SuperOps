import React, { useEffect, useState } from 'react'
import {
  View,
  Text,
  TextInput,
  Pressable,
  SafeAreaView,
  ScrollView,
  Alert,
  ActivityIndicator,
} from 'react-native'
import { theme, avatarColor } from '../lib/theme'
import { userApi } from '../api/users'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'

// --- Small reusable presentational helpers -------------------------------

function Section({ title, children }: { title: string; children: React.ReactNode }) {
  return (
    <View style={{ marginBottom: 28 }}>
      <Text
        style={{
          color: theme.textMuted,
          fontSize: 11,
          fontWeight: '700',
          letterSpacing: 1,
          marginBottom: 10,
          paddingHorizontal: 4,
        }}
      >
        {title.toUpperCase()}
      </Text>
      <View
        style={{
          backgroundColor: theme.surface,
          borderWidth: 1,
          borderColor: theme.border,
          borderRadius: 12,
          padding: 16,
        }}
      >
        {children}
      </View>
    </View>
  )
}

function Field({
  label,
  value,
  onChangeText,
  placeholder,
  secureTextEntry,
  autoCapitalize,
  keyboardType,
}: {
  label: string
  value: string
  onChangeText: (t: string) => void
  placeholder?: string
  secureTextEntry?: boolean
  autoCapitalize?: 'none' | 'sentences' | 'words' | 'characters'
  keyboardType?: 'default' | 'numeric'
}) {
  return (
    <View style={{ marginBottom: 12 }}>
      <Text style={{ color: theme.textFaint, fontSize: 12, marginBottom: 6 }}>{label}</Text>
      <TextInput
        value={value}
        onChangeText={onChangeText}
        placeholder={placeholder}
        placeholderTextColor={theme.textFaint}
        secureTextEntry={secureTextEntry}
        autoCapitalize={autoCapitalize ?? 'none'}
        keyboardType={keyboardType ?? 'default'}
        style={{
          backgroundColor: theme.bg,
          borderWidth: 1,
          borderColor: theme.borderStrong,
          borderRadius: 10,
          paddingHorizontal: 12,
          paddingVertical: 10,
          color: theme.text,
          fontSize: 15,
        }}
      />
    </View>
  )
}

function Button({
  label,
  onPress,
  loading,
  variant,
}: {
  label: string
  onPress: () => void
  loading?: boolean
  variant?: 'primary' | 'danger' | 'ghost'
}) {
  const bg =
    variant === 'danger' ? theme.danger : variant === 'ghost' ? 'transparent' : theme.primary
  const borderColor = variant === 'ghost' ? theme.borderStrong : bg
  const textColor = variant === 'ghost' ? theme.body : theme.primaryText
  return (
    <Pressable
      onPress={onPress}
      disabled={loading}
      style={{
        backgroundColor: bg,
        borderWidth: 1,
        borderColor,
        borderRadius: 10,
        paddingVertical: 12,
        alignItems: 'center',
        opacity: loading ? 0.6 : 1,
      }}
    >
      {loading ? (
        <ActivityIndicator color={textColor} />
      ) : (
        <Text style={{ color: textColor, fontSize: 15, fontWeight: '600' }}>{label}</Text>
      )}
    </Pressable>
  )
}

// --- Screen --------------------------------------------------------------

export default function SettingsScreen({ navigation }: { navigation: any; route: any }) {
  const user = useAuthStore((s) => s.user)
  const setUser = useAuthStore((s) => s.setUser)
  const logout = useAuthStore((s) => s.logout)

  // Profile
  const [fullName, setFullName] = useState(user?.full_name ?? '')
  const [savingProfile, setSavingProfile] = useState(false)

  // Custom status
  const [statusText, setStatusText] = useState(user?.status_text ?? '')
  const [statusEmoji, setStatusEmoji] = useState(user?.status_emoji ?? '')
  const [savingStatus, setSavingStatus] = useState(false)

  // Change password
  const [oldPassword, setOldPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [changingPw, setChangingPw] = useState(false)

  // 2FA
  const [totpEnabled, setTotpEnabled] = useState<boolean | null>(null)
  const [totpLoading, setTotpLoading] = useState(false)
  const [setupSecret, setSetupSecret] = useState<string | null>(null)
  const [setupUrl, setSetupUrl] = useState<string | null>(null)
  const [totpCode, setTotpCode] = useState('')
  const [backupCodes, setBackupCodes] = useState<string[] | null>(null)
  const [disableCode, setDisableCode] = useState('')

  useEffect(() => {
    authApi
      .totpStatus()
      .then((res) => setTotpEnabled(res.data?.enabled ?? false))
      .catch(() => setTotpEnabled(false))
  }, [])

  const saveProfile = async () => {
    if (!fullName.trim()) {
      Alert.alert('Validation', 'Full name cannot be empty')
      return
    }
    setSavingProfile(true)
    try {
      const res = await userApi.updateMe({ full_name: fullName.trim() })
      if (res.data) setUser(res.data)
      Alert.alert('Saved', 'Your profile has been updated')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to update profile')
    } finally {
      setSavingProfile(false)
    }
  }

  const saveStatus = async () => {
    setSavingStatus(true)
    try {
      await userApi.updateStatus({ status_text: statusText.trim(), status_emoji: statusEmoji.trim() })
      if (user) setUser({ ...user, status_text: statusText.trim(), status_emoji: statusEmoji.trim() })
      Alert.alert('Saved', 'Your status has been updated')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to update status')
    } finally {
      setSavingStatus(false)
    }
  }

  const clearStatus = async () => {
    setSavingStatus(true)
    try {
      await userApi.updateStatus({ status_text: '', status_emoji: '' })
      setStatusText('')
      setStatusEmoji('')
      if (user) setUser({ ...user, status_text: '', status_emoji: '' })
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to clear status')
    } finally {
      setSavingStatus(false)
    }
  }

  const changePassword = async () => {
    if (!oldPassword || !newPassword) {
      Alert.alert('Validation', 'Both current and new password are required')
      return
    }
    if (newPassword.length < 8) {
      Alert.alert('Validation', 'New password must be at least 8 characters')
      return
    }
    setChangingPw(true)
    try {
      await authApi.changePassword({ old_password: oldPassword, new_password: newPassword })
      setOldPassword('')
      setNewPassword('')
      Alert.alert('Done', 'Your password has been changed')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to change password')
    } finally {
      setChangingPw(false)
    }
  }

  const startTotpSetup = async () => {
    setTotpLoading(true)
    try {
      const res = await authApi.totpSetup()
      setSetupSecret(res.data?.secret ?? '')
      setSetupUrl(res.data?.otpauth_url ?? '')
      setBackupCodes(null)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to start 2FA setup')
    } finally {
      setTotpLoading(false)
    }
  }

  const verifyTotp = async () => {
    if (totpCode.trim().length !== 6) {
      Alert.alert('Validation', 'Enter the 6-digit code from your authenticator app')
      return
    }
    setTotpLoading(true)
    try {
      const res = await authApi.totpVerify(totpCode.trim())
      setBackupCodes(res.data?.backup_codes ?? [])
      setTotpEnabled(true)
      setSetupSecret(null)
      setSetupUrl(null)
      setTotpCode('')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Invalid code')
    } finally {
      setTotpLoading(false)
    }
  }

  const cancelTotpSetup = () => {
    setSetupSecret(null)
    setSetupUrl(null)
    setTotpCode('')
  }

  const disableTotp = async () => {
    if (disableCode.trim().length !== 6) {
      Alert.alert('Validation', 'Enter the 6-digit code to disable 2FA')
      return
    }
    setTotpLoading(true)
    try {
      await authApi.totpDisable(disableCode.trim())
      setTotpEnabled(false)
      setDisableCode('')
      setBackupCodes(null)
      Alert.alert('Done', 'Two-factor authentication has been disabled')
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to disable 2FA')
    } finally {
      setTotpLoading(false)
    }
  }

  const confirmLogout = () => {
    Alert.alert('Log out', 'Are you sure you want to log out?', [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Log out', style: 'destructive', onPress: () => logout() },
    ])
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
        <Pressable onPress={() => navigation.goBack()} hitSlop={12} style={{ marginRight: 12 }}>
          <Text style={{ color: theme.accent, fontSize: 16 }}>‹ Back</Text>
        </Pressable>
        <Text style={{ color: theme.text, fontSize: 17, fontWeight: '700', flex: 1 }}>Settings</Text>
      </View>

      <ScrollView contentContainerStyle={{ padding: 16, paddingBottom: 48 }}>
        {/* Account identity */}
        <View style={{ flexDirection: 'row', alignItems: 'center', marginBottom: 28 }}>
          <View
            style={{
              width: 56,
              height: 56,
              borderRadius: 28,
              backgroundColor: user ? avatarColor(user.id) : theme.borderStrong,
              alignItems: 'center',
              justifyContent: 'center',
              marginRight: 14,
            }}
          >
            <Text style={{ color: '#fff', fontSize: 22, fontWeight: '700' }}>
              {(user?.full_name || user?.username || '?').charAt(0).toUpperCase()}
            </Text>
          </View>
          <View style={{ flex: 1 }}>
            <Text style={{ color: theme.text, fontSize: 18, fontWeight: '700' }}>
              {user?.full_name || user?.username || 'Unknown'}
            </Text>
            <Text style={{ color: theme.textMuted, fontSize: 13 }}>{user?.email || ''}</Text>
          </View>
        </View>

        {/* Profile */}
        <Section title="Profile">
          <Field label="Full name" value={fullName} onChangeText={setFullName} placeholder="Your name" autoCapitalize="words" />
          <Button label="Save profile" onPress={saveProfile} loading={savingProfile} />
        </Section>

        {/* Custom status */}
        <Section title="Custom status">
          <Field label="Emoji" value={statusEmoji} onChangeText={setStatusEmoji} placeholder=":wave:" />
          <Field label="Status text" value={statusText} onChangeText={setStatusText} placeholder="What's happening?" autoCapitalize="sentences" />
          <Button label="Save status" onPress={saveStatus} loading={savingStatus} />
          <View style={{ height: 8 }} />
          <Button label="Clear status" onPress={clearStatus} variant="ghost" />
        </Section>

        {/* Change password */}
        <Section title="Change password">
          <Field label="Current password" value={oldPassword} onChangeText={setOldPassword} secureTextEntry placeholder="••••••••" />
          <Field label="New password" value={newPassword} onChangeText={setNewPassword} secureTextEntry placeholder="At least 8 characters" />
          <Button label="Update password" onPress={changePassword} loading={changingPw} />
        </Section>

        {/* Two-factor auth */}
        <Section title="Two-factor authentication">
          {totpEnabled === null ? (
            <ActivityIndicator color={theme.accent} />
          ) : backupCodes ? (
            // One-time backup codes display (after verify)
            <View>
              <Text style={{ color: theme.success, fontSize: 15, fontWeight: '700', marginBottom: 8 }}>
                2FA enabled
              </Text>
              <Text style={{ color: theme.textMuted, fontSize: 13, marginBottom: 12 }}>
                Save these backup codes somewhere safe. Each can be used once if you lose access to your
                authenticator. They will not be shown again.
              </Text>
              <View
                style={{
                  backgroundColor: theme.bg,
                  borderWidth: 1,
                  borderColor: theme.borderStrong,
                  borderRadius: 10,
                  padding: 12,
                }}
              >
                {backupCodes.map((code) => (
                  <Text
                    key={code}
                    selectable
                    style={{ color: theme.body, fontSize: 15, fontFamily: 'Courier', marginBottom: 4 }}
                  >
                    {code}
                  </Text>
                ))}
              </View>
              <View style={{ height: 12 }} />
              <Button label="I saved my codes" onPress={() => setBackupCodes(null)} variant="ghost" />
            </View>
          ) : totpEnabled ? (
            // Enabled — offer disable
            <View>
              <Text style={{ color: theme.success, fontSize: 15, fontWeight: '600', marginBottom: 4 }}>
                Enabled
              </Text>
              <Text style={{ color: theme.textMuted, fontSize: 13, marginBottom: 12 }}>
                Enter a 6-digit code from your authenticator app to turn off 2FA.
              </Text>
              <Field
                label="Authenticator code"
                value={disableCode}
                onChangeText={setDisableCode}
                placeholder="000000"
                keyboardType="numeric"
              />
              <Button label="Disable 2FA" onPress={disableTotp} loading={totpLoading} variant="danger" />
            </View>
          ) : setupSecret !== null ? (
            // Mid-setup — show secret + url + verify input
            <View>
              <Text style={{ color: theme.body, fontSize: 14, marginBottom: 8 }}>
                Scan this in your authenticator app, or add the secret manually.
              </Text>
              <Text style={{ color: theme.textFaint, fontSize: 12, marginBottom: 4 }}>Secret</Text>
              <View
                style={{
                  backgroundColor: theme.bg,
                  borderWidth: 1,
                  borderColor: theme.borderStrong,
                  borderRadius: 10,
                  padding: 12,
                  marginBottom: 12,
                }}
              >
                <Text selectable style={{ color: theme.text, fontSize: 15, fontFamily: 'Courier' }}>
                  {setupSecret}
                </Text>
              </View>
              <Text style={{ color: theme.textFaint, fontSize: 12, marginBottom: 4 }}>otpauth URL</Text>
              <View
                style={{
                  backgroundColor: theme.bg,
                  borderWidth: 1,
                  borderColor: theme.borderStrong,
                  borderRadius: 10,
                  padding: 12,
                  marginBottom: 12,
                }}
              >
                <Text selectable style={{ color: theme.textMuted, fontSize: 12 }}>
                  {setupUrl}
                </Text>
              </View>
              <Field
                label="Enter the 6-digit code"
                value={totpCode}
                onChangeText={setTotpCode}
                placeholder="000000"
                keyboardType="numeric"
              />
              <Button label="Verify & enable" onPress={verifyTotp} loading={totpLoading} />
              <View style={{ height: 8 }} />
              <Button label="Cancel" onPress={cancelTotpSetup} variant="ghost" />
            </View>
          ) : (
            // Disabled — offer setup
            <View>
              <Text style={{ color: theme.textMuted, fontSize: 13, marginBottom: 12 }}>
                Add an extra layer of security by requiring a code from an authenticator app when you sign
                in.
              </Text>
              <Button label="Enable 2FA" onPress={startTotpSetup} loading={totpLoading} />
            </View>
          )}
        </Section>

        {/* Logout */}
        <Button label="Log out" onPress={confirmLogout} variant="danger" />
      </ScrollView>
    </SafeAreaView>
  )
}
