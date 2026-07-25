import React, { useState } from 'react'
import { View, Text, Pressable, SafeAreaView, Alert, ScrollView } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { workspaceApi } from '../api/workspaces'
import { adminApi, type CreatedInvitation } from '../api/admin'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useAuthStore } from '../stores/authStore'
import { errorMessage } from '../api/client'
import { theme } from '../lib/theme'
import { Button, Field, MIN_TOUCH } from './internal/ui'

type Props = NativeStackScreenProps<RootStackParamList, 'Onboarding'>

function slugify(name: string): string {
  return name
    .toLowerCase()
    .trim()
    .replace(/[^a-z0-9\s-]/g, '')
    .replace(/\s+/g, '-')
    .replace(/-+/g, '-')
    .replace(/^-+|-+$/g, '')
    .slice(0, 40)
}

export default function OnboardingScreen({ navigation }: Props) {
  const [step, setStep] = useState<1 | 2>(1)
  const [wsName, setWsName] = useState('My Team')
  const [emails, setEmails] = useState('')
  const [loading, setLoading] = useState(false)
  const [created, setCreated] = useState<CreatedInvitation[]>([])

  const workspaces = useWorkspaceStore((s) => s.workspaces)
  const activeWorkspace = useWorkspaceStore((s) => s.activeWorkspace)

  /**
   * Step 1 used to be a no-op: it only renamed a workspace the user already
   * had, and the screen is reached precisely when they have none. Nothing in
   * the app called POST /workspaces, so the account was unusable.
   */
  const handleCreateWorkspace = async () => {
    const name = wsName.trim()
    const slug = slugify(name)
    if (!name || !slug) {
      Alert.alert('Validation', 'Enter a workspace name using letters or numbers.')
      return
    }
    setLoading(true)
    try {
      const existing = await workspaceApi.list()
      const list = existing.data ?? []
      if (list.length > 0) {
        // Returning user who somehow landed here: rename rather than pile up
        // workspaces they did not ask for.
        const ws = list[0]
        const updated = await workspaceApi.update(ws.id, { name })
        const next = list.map((w) => (w.id === ws.id ? updated.data : w))
        useWorkspaceStore.getState().setWorkspaces(next)
        useWorkspaceStore.getState().setActiveWorkspace(updated.data)
      } else {
        const res = await workspaceApi.create({ name, slug })
        useWorkspaceStore.getState().setWorkspaces([res.data])
        useWorkspaceStore.getState().setActiveWorkspace(res.data)
      }
      setStep(2)
    } catch (err) {
      Alert.alert('Error', errorMessage(err, 'Could not create your workspace'))
    } finally {
      setLoading(false)
    }
  }

  /**
   * One failed address used to abort the loop and report total failure even
   * when earlier invites had already been sent. Every address is now attempted
   * and the outcome is reported per address.
   */
  const handleInvite = async () => {
    const ws = useWorkspaceStore.getState().activeWorkspace
    if (!ws) {
      Alert.alert('Error', 'Create your workspace first.')
      setStep(1)
      return
    }
    const emailList = Array.from(
      new Set(
        emails
          .split(/[,\n\s]+/)
          .map((e) => e.trim().toLowerCase())
          .filter(Boolean),
      ),
    )
    if (emailList.length === 0) {
      finishOnboarding()
      return
    }

    setLoading(true)
    try {
      const results = await Promise.allSettled(
        emailList.map((email) =>
          adminApi.createInvitation({ workspace_id: ws.id, email, role: 'member' }),
        ),
      )
      const ok: CreatedInvitation[] = []
      const failed: string[] = []
      results.forEach((r, i) => {
        if (r.status === 'fulfilled') ok.push(r.value.data)
        else failed.push(`${emailList[i]} — ${errorMessage(r.reason, 'failed')}`)
      })
      setCreated(ok)
      if (failed.length === 0) {
        Alert.alert('Invited', `${ok.length} invitation${ok.length === 1 ? '' : 's'} created.`)
      } else {
        Alert.alert(
          ok.length > 0 ? 'Partly sent' : 'Nothing sent',
          `${ok.length} created, ${failed.length} failed:\n\n${failed.join('\n')}`,
        )
      }
    } finally {
      setLoading(false)
    }
  }

  const finishOnboarding = () => {
    const ws = useWorkspaceStore.getState().activeWorkspace
    if (!ws) {
      Alert.alert('Almost there', 'Create your workspace before continuing.')
      setStep(1)
      return
    }
    navigation.replace('Workspace', { workspaceId: ws.id })
  }

  const confirmLogout = () => {
    Alert.alert('Log out', 'Are you sure you want to log out?', [
      { text: 'Cancel', style: 'cancel' },
      { text: 'Log out', style: 'destructive', onPress: () => void useAuthStore.getState().logout() },
    ])
  }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: theme.bg }}>
      <View
        style={{
          minHeight: 56,
          paddingHorizontal: 16,
          flexDirection: 'row',
          alignItems: 'center',
          justifyContent: 'flex-end',
        }}
      >
        {/* There was no way out of this screen other than deleting the app. */}
        <Pressable
          onPress={confirmLogout}
          accessibilityRole="button"
          accessibilityLabel="Log out"
          hitSlop={8}
          style={{ minHeight: MIN_TOUCH, justifyContent: 'center', paddingHorizontal: 8 }}
        >
          <Text style={{ color: theme.textMuted, fontSize: 14 }}>Log out</Text>
        </Pressable>
      </View>

      <ScrollView
        contentContainerStyle={{ flexGrow: 1, justifyContent: 'center', paddingHorizontal: 32, paddingBottom: 40 }}
        keyboardShouldPersistTaps="handled"
      >
        <View
          accessible
          accessibilityRole="progressbar"
          accessibilityLabel={`Step ${step} of 2`}
          style={{ flexDirection: 'row', justifyContent: 'center', gap: 8, marginBottom: 40 }}
        >
          {[1, 2].map((s) => (
            <View
              key={s}
              style={{ width: 40, height: 4, borderRadius: 2, backgroundColor: s <= step ? theme.primary : theme.borderStrong }}
            />
          ))}
        </View>

        {step === 1 ? (
          <>
            <Text accessibilityRole="header" style={{ color: theme.text, fontSize: 24, fontWeight: 'bold', textAlign: 'center' }}>
              {workspaces.length > 0 ? 'Name your workspace' : 'Create your workspace'}
            </Text>
            <Text style={{ color: theme.textMuted, textAlign: 'center', marginTop: 4, marginBottom: 32 }}>
              This is your team's home in SuperOps
            </Text>

            <Field
              label="Workspace name"
              value={wsName}
              onChangeText={setWsName}
              placeholder="Acme Inc"
              autoCapitalize="words"
              hint="Used for the workspace URL as well"
              maxLength={60}
            />
            <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 16 }}>
              URL: <Text style={{ color: theme.body }}>#{slugify(wsName) || '…'}</Text>
            </Text>

            <Button
              label={workspaces.length > 0 ? 'Continue' : 'Create workspace'}
              onPress={handleCreateWorkspace}
              loading={loading}
              disabled={!slugify(wsName)}
            />
          </>
        ) : (
          <>
            <Text accessibilityRole="header" style={{ color: theme.text, fontSize: 24, fontWeight: 'bold', textAlign: 'center' }}>
              Invite your team
            </Text>
            <Text style={{ color: theme.textMuted, textAlign: 'center', marginTop: 4, marginBottom: 24 }}>
              {activeWorkspace ? `Adding people to ${activeWorkspace.name}` : 'Enter email addresses, one per line'}
            </Text>

            <Field
              label="Email addresses"
              value={emails}
              onChangeText={setEmails}
              placeholder={'alice@company.com\nbob@company.com'}
              multiline
              keyboardType="email-address"
              hint="One address per line, or separated by commas"
            />

            <Button
              label="Send invites & continue"
              onPress={handleInvite}
              loading={loading}
            />
            <View style={{ height: 12 }} />
            <Button label="Skip for now" onPress={finishOnboarding} variant="ghost" />

            {created.length > 0 ? (
              <View
                style={{
                  marginTop: 24,
                  backgroundColor: theme.surface,
                  borderWidth: 1,
                  borderColor: theme.border,
                  borderRadius: 12,
                  padding: 16,
                }}
              >
                <Text style={{ color: theme.text, fontSize: 14, fontWeight: '700', marginBottom: 4 }}>
                  Share these links
                </Text>
                <Text style={{ color: theme.textMuted, fontSize: 12, marginBottom: 12 }}>
                  SuperOps does not send email. Each link is shown once — copy it now.
                </Text>
                {created.map((inv) => (
                  <View key={inv.id} style={{ marginBottom: 10 }}>
                    <Text style={{ color: theme.body, fontSize: 13, fontWeight: '600' }}>{inv.email}</Text>
                    <Text selectable style={{ color: theme.accent, fontSize: 12, fontFamily: 'Courier' }}>
                      {inv.invite_url}
                    </Text>
                  </View>
                ))}
              </View>
            ) : null}
          </>
        )}
      </ScrollView>
    </SafeAreaView>
  )
}
