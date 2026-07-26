import React, { useEffect, useState } from 'react'
import {
  View,
  Text,
  Pressable,
  SafeAreaView,
  ScrollView,
  Alert,
  ActivityIndicator,
} from 'react-native'
import { theme, avatarColor, presenceColor } from '../lib/theme'
import { userApi } from '../api/users'
import { authApi } from '../api/auth'
import { useAuthStore } from '../stores/authStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUiStore } from '../stores/uiStore'
import { wsManager } from '../lib/websocket'
import { errorMessage } from '../api/client'
import type { PresenceStatus } from '../lib/types'
import QrCode from './internal/QrCode'
import { useWorkspaceRole } from './internal/useWorkspaceRole'
import { useResponsive } from '../lib/responsive'
import { Button, Chip, CONTENT_MAX_WIDTH, Field, ScreenHeader, Section, contentColumn } from './internal/ui'

const PRESENCE_OPTIONS: Array<{ key: PresenceStatus; label: string }> = [
  { key: 'online', label: 'Active' },
  { key: 'away', label: 'Away' },
  { key: 'dnd', label: 'Do not disturb' },
]

function NavRow({ label, detail, onPress }: { label: string; detail?: string; onPress: () => void }) {
  const { minTouch } = useResponsive()
  return (
    <Pressable
      onPress={onPress}
      accessibilityRole="button"
      accessibilityLabel={detail ? `${label}, ${detail}` : label}
      style={{ flexDirection: 'row', alignItems: 'center', minHeight: minTouch, paddingVertical: 10 }}
    >
      <Text style={{ color: theme.body, fontSize: 15, flex: 1 }}>{label}</Text>
      {detail ? <Text style={{ color: theme.textMuted, fontSize: 13, marginRight: 6 }}>{detail}</Text> : null}
      <Text style={{ color: theme.textMuted, fontSize: 18 }}>›</Text>
    </Pressable>
  )
}

export default function SettingsScreen({ navigation }: { navigation: any; route: any }) {
  const user = useAuthStore((s) => s.user)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)
  const presence = useUiStore((s) => s.presence)
  const { tier, gutter } = useResponsive()
  // Paired actions stack under a thumb and sit side by side once there is room.
  const actionRow = {
    flexDirection: (tier === 'compact' ? 'column' : 'row') as 'column' | 'row',
    gap: tier === 'compact' ? 8 : 12,
  }

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

  /**
   * AdminScreen was registered in the navigator and reachable from nowhere.
   * Show the entry only when the caller really administers this workspace —
   * `/admin/*` answers 403 to everyone else.
   */
  const { isAdmin: isWorkspaceAdmin } = useWorkspaceRole()

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
      if (res.data) await useAuthStore.getState().setUser(res.data)
      Alert.alert('Saved', 'Your profile has been updated')
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to update profile'))
    } finally {
      setSavingProfile(false)
    }
  }

  /**
   * status_text / status_emoji round-trip through the API now, so the stored
   * user is updated from the response instead of from what we hoped we sent.
   */
  const writeStatus = async (text: string, emoji: string) => {
    setSavingStatus(true)
    try {
      const res = await userApi.updateStatus({ status_text: text, status_emoji: emoji })
      setStatusText(res.data.status_text)
      setStatusEmoji(res.data.status_emoji)
      const current = useAuthStore.getState().user
      if (current) {
        await useAuthStore.getState().setUser({
          ...current,
          status_text: res.data.status_text,
          status_emoji: res.data.status_emoji,
        })
      }
      return true
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Failed to update status'))
      return false
    } finally {
      setSavingStatus(false)
    }
  }

  const saveStatus = async () => {
    if (await writeStatus(statusText.trim(), statusEmoji.trim())) {
      Alert.alert('Saved', 'Your status has been updated')
    }
  }

  const clearStatus = () => void writeStatus('', '')

  const setPresenceStatus = (status: PresenceStatus) => {
    // Presence is a realtime-only concept; there is no REST route for it.
    wsManager.setPresence(status)
    if (user) useUiStore.getState().setUserPresence(user.id, status)
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
      Alert.alert('Error', errorMessage(err, 'Failed to change password'))
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
      Alert.alert('Error', errorMessage(err, 'Failed to start 2FA setup'))
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
      Alert.alert('Error', errorMessage(err, 'Invalid code'))
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
      Alert.alert('Error', errorMessage(err, 'Failed to disable 2FA'))
    } finally {
      setTotpLoading(false)
    }
  }

  const confirmLogout = () => {
    Alert.alert('Log out', 'Are you sure you want to log out?', [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Log out', style: 'destructive', onPress: () => void useAuthStore.getState().logout() },
    ])
  }

  const myPresence: PresenceStatus = (user ? presence[user.id] : undefined) ?? 'online'

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <ScreenHeader title="Settings" onBack={() => navigation.goBack()} maxWidth={CONTENT_MAX_WIDTH} />

      {/* One column, capped at the reading measure. These are three-field forms:
          spreading them into columns on a monitor would be worse, not better. */}
      <ScrollView
        contentContainerStyle={{ ...contentColumn(), paddingHorizontal: gutter, paddingTop: 16, paddingBottom: 48 }}
      >
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
            <Text accessibilityRole="header" style={{ color: theme.text, fontSize: 18, fontWeight: '700' }}>
              {user?.full_name || user?.username || 'Unknown'}
            </Text>
            <Text style={{ color: theme.textMuted, fontSize: 13 }}>{user?.email || ''}</Text>
          </View>
          <View
            accessible
            accessibilityLabel={`Presence: ${myPresence}`}
            style={{
              width: 12,
              height: 12,
              borderRadius: 6,
              backgroundColor: presenceColor[myPresence] ?? presenceColor.offline,
            }}
          />
        </View>

        {/* Availability */}
        <Section title="Availability">
          <View style={{ flexDirection: 'row', flexWrap: 'wrap', gap: 8 }}>
            {PRESENCE_OPTIONS.map((p) => (
              <Chip
                key={p.key}
                label={p.label}
                selected={myPresence === p.key}
                onPress={() => setPresenceStatus(p.key)}
                accessibilityLabel={`Set availability to ${p.label}`}
              />
            ))}
          </View>
        </Section>

        {/* Profile */}
        <Section title="Profile">
          <Field
            label="Full name"
            value={fullName}
            onChangeText={setFullName}
            placeholder="Your name"
            autoCapitalize="words"
          />
          <Button label="Save profile" onPress={saveProfile} loading={savingProfile} inline />
        </Section>

        {/* Custom status */}
        <Section title="Custom status">
          <Field label="Emoji" value={statusEmoji} onChangeText={setStatusEmoji} placeholder="🎧" maxLength={8} />
          <Field
            label="Status text"
            value={statusText}
            onChangeText={setStatusText}
            placeholder="What's happening?"
            autoCapitalize="sentences"
            maxLength={100}
          />
          <View style={actionRow}>
            <Button label="Save status" onPress={saveStatus} loading={savingStatus} inline />
            <Button label="Clear status" onPress={clearStatus} variant="ghost" inline />
          </View>
        </Section>

        {/* Saved items / admin */}
        <Section title="Your stuff">
          <NavRow label="Work" onPress={() => navigation.navigate('Board')} />
          <NavRow label="Drive" onPress={() => navigation.navigate('Drive')} />
          <NavRow label="Bookmarks" onPress={() => navigation.navigate('Bookmarks')} />
          {/* The shared inbox and automation. Both were registered, routed and
              reachable ONLY by typing a URL — which on native, where the prefix
              is superops://, means not reachable at all. A screen with no way
              in is a feature that does not exist. */}
          <NavRow label="Inbox" onPress={() => navigation.navigate('Inbox')} />
          {/* Automation is an ADMIN surface on both sides. A workflow's steps
              name private channels and carry the literal bodies it posts, and
              the server refuses a non-admin — so offering the row to everyone
              would be a link to a permission error. */}
          {isWorkspaceAdmin ? (
            <NavRow label="Automation" onPress={() => navigation.navigate('Workflows')} />
          ) : null}
          {isWorkspaceAdmin ? (
            <NavRow label="Email setup" onPress={() => navigation.navigate('MailSetup')} />
          ) : null}
          {isWorkspaceAdmin ? (
            <NavRow
              label="Admin panel"
              detail={activeWorkspace?.name}
              onPress={() => navigation.navigate('Admin')}
            />
          ) : null}
        </Section>

        {/* Change password */}
        <Section title="Change password">
          <Field
            label="Current password"
            value={oldPassword}
            onChangeText={setOldPassword}
            secureTextEntry
            placeholder="••••••••"
          />
          <Field
            label="New password"
            value={newPassword}
            onChangeText={setNewPassword}
            secureTextEntry
            placeholder="At least 8 characters"
          />
          <Button label="Update password" onPress={changePassword} loading={changingPw} inline />
        </Section>

        {/* Two-factor auth */}
        <Section title="Two-factor authentication">
          {totpEnabled === null ? (
            <ActivityIndicator color={theme.accent} accessibilityLabel="Checking two-factor status" />
          ) : backupCodes ? (
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
              <Button label="I saved my codes" onPress={() => setBackupCodes(null)} variant="ghost" inline />
            </View>
          ) : totpEnabled ? (
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
                keyboardType="number-pad"
                maxLength={6}
              />
              <Button label="Disable 2FA" onPress={disableTotp} loading={totpLoading} variant="danger" inline />
            </View>
          ) : setupSecret !== null ? (
            <View>
              <Text style={{ color: theme.body, fontSize: 14, marginBottom: 12 }}>
                Scan this with your authenticator app, or add the secret manually.
              </Text>

              {/* The instruction used to be unfollowable: only the base32 secret
                  and the raw otpauth URL were shown, with nothing to scan. */}
              {setupUrl ? (
                <View style={{ alignItems: 'center', marginBottom: 16 }}>
                  <QrCode value={setupUrl} size={220} accessibilityLabel="Two-factor setup QR code" />
                </View>
              ) : null}

              <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 4 }}>Secret</Text>
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
                <Text
                  selectable
                  accessibilityLabel={`Setup secret ${setupSecret.split('').join(' ')}`}
                  style={{ color: theme.text, fontSize: 15, fontFamily: 'Courier' }}
                >
                  {setupSecret}
                </Text>
              </View>
              <Field
                label="Enter the 6-digit code"
                value={totpCode}
                onChangeText={setTotpCode}
                placeholder="000000"
                keyboardType="number-pad"
                maxLength={6}
              />
              <View style={actionRow}>
                <Button label="Verify & enable" onPress={verifyTotp} loading={totpLoading} inline />
                <Button label="Cancel" onPress={cancelTotpSetup} variant="ghost" inline />
              </View>
            </View>
          ) : (
            <View>
              <Text style={{ color: theme.textMuted, fontSize: 13, marginBottom: 12 }}>
                Add an extra layer of security by requiring a code from an authenticator app when you sign in.
              </Text>
              <Button label="Enable 2FA" onPress={startTotpSetup} loading={totpLoading} inline />
            </View>
          )}
        </Section>

        <Button label="Log out" onPress={confirmLogout} variant="danger" inline />
      </ScrollView>
    </SafeAreaView>
  )
}
