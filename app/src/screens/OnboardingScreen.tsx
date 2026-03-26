import React, { useState } from 'react'
import { View, Text, TextInput, Pressable, SafeAreaView, Alert, ScrollView } from 'react-native'
import type { NativeStackScreenProps } from '@react-navigation/native-stack'
import type { RootStackParamList } from '../navigation/AppNavigator'
import { workspaceApi } from '../api/workspaces'
import { api } from '../api/client'
import { useWorkspaceStore } from '../stores/workspaceStore'

type Props = NativeStackScreenProps<RootStackParamList, 'Onboarding'>

export default function OnboardingScreen({ navigation }: Props) {
  const [step, setStep] = useState(1)
  const [wsName, setWsName] = useState('My Team')
  const [emails, setEmails] = useState('')
  const [loading, setLoading] = useState(false)
  const { setWorkspaces, setActiveWorkspace } = useWorkspaceStore()

  const handleUpdateWorkspace = async () => {
    setLoading(true)
    try {
      const wsList = await workspaceApi.list()
      if (wsList.data.length > 0) {
        const ws = wsList.data[0]
        await workspaceApi.update(ws.id, { name: wsName })
        setWorkspaces(wsList.data.map((w) => w.id === ws.id ? { ...w, name: wsName } : w))
        setActiveWorkspace({ ...ws, name: wsName })
      }
      setStep(2)
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed')
    } finally {
      setLoading(false)
    }
  }

  const handleInvite = async () => {
    const emailList = emails.split(/[,\n]/).map((e) => e.trim()).filter(Boolean)
    if (emailList.length === 0) {
      finishOnboarding()
      return
    }

    setLoading(true)
    try {
      for (const email of emailList) {
        await api.post('/admin/invitations', { email, role: 'member' })
      }
      Alert.alert('Invited!', `${emailList.length} invitation(s) sent.`)
      finishOnboarding()
    } catch (err) {
      Alert.alert('Error', err instanceof Error ? err.message : 'Failed to invite')
    } finally {
      setLoading(false)
    }
  }

  const finishOnboarding = () => {
    const ws = useWorkspaceStore.getState().activeWorkspace
    if (ws) {
      navigation.replace('Workspace', { workspaceId: ws.id })
    }
  }

  const buttonStyle = { backgroundColor: '#4f46e5', borderRadius: 12, padding: 14, alignItems: 'center' as const, marginTop: 16, opacity: loading ? 0.6 : 1 }

  return (
    <SafeAreaView style={{ flex: 1, backgroundColor: '#020617' }}>
      <ScrollView contentContainerStyle={{ flex: 1, justifyContent: 'center', paddingHorizontal: 32 }}>
        {/* Progress */}
        <View style={{ flexDirection: 'row', justifyContent: 'center', gap: 8, marginBottom: 40 }}>
          {[1, 2].map((s) => (
            <View key={s} style={{ width: 40, height: 4, borderRadius: 2, backgroundColor: s <= step ? '#4f46e5' : '#334155' }} />
          ))}
        </View>

        {step === 1 && (
          <>
            <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold', textAlign: 'center' }}>Name your workspace</Text>
            <Text style={{ color: '#94a3b8', textAlign: 'center', marginTop: 4, marginBottom: 32 }}>This is your team's home in SuperOps</Text>

            <TextInput value={wsName} onChangeText={setWsName}
              style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15 }}
            />

            <Pressable onPress={handleUpdateWorkspace} disabled={loading} style={buttonStyle}>
              <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>Continue</Text>
            </Pressable>
          </>
        )}

        {step === 2 && (
          <>
            <Text style={{ color: '#fff', fontSize: 24, fontWeight: 'bold', textAlign: 'center' }}>Invite your team</Text>
            <Text style={{ color: '#94a3b8', textAlign: 'center', marginTop: 4, marginBottom: 32 }}>Enter email addresses (one per line)</Text>

            <TextInput
              value={emails} onChangeText={setEmails}
              placeholder={"alice@company.com\nbob@company.com"} placeholderTextColor="#64748b"
              multiline numberOfLines={4}
              style={{ backgroundColor: '#0f172a', borderWidth: 1, borderColor: '#334155', borderRadius: 12, padding: 14, color: '#fff', fontSize: 15, minHeight: 120, textAlignVertical: 'top' }}
            />

            <Pressable onPress={handleInvite} disabled={loading} style={buttonStyle}>
              <Text style={{ color: '#fff', fontSize: 15, fontWeight: '600' }}>{loading ? 'Sending...' : 'Send Invites & Continue'}</Text>
            </Pressable>

            <Pressable onPress={finishOnboarding} style={{ marginTop: 16, alignItems: 'center' }}>
              <Text style={{ color: '#64748b', fontSize: 14 }}>Skip for now</Text>
            </Pressable>
          </>
        )}
      </ScrollView>
    </SafeAreaView>
  )
}
