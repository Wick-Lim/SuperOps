import React, { useEffect, useState } from 'react'
import { NavigationContainer } from '@react-navigation/native'
import { createNativeStackNavigator } from '@react-navigation/native-stack'
import { useAuthStore } from '../stores/authStore'
import { workspaceApi } from '../api/workspaces'
import { useWorkspaceStore } from '../stores/workspaceStore'
import LoginScreen from '../screens/LoginScreen'
import InviteScreen from '../screens/InviteScreen'
import OnboardingScreen from '../screens/OnboardingScreen'
import WorkspaceScreen from '../screens/WorkspaceScreen'

export type RootStackParamList = {
  Login: undefined
  Invite: { token?: string }
  Onboarding: undefined
  Workspace: { workspaceId: string }
}

const Stack = createNativeStackNavigator<RootStackParamList>()

export default function AppNavigator() {
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated)
  const [initialRoute, setInitialRoute] = useState<keyof RootStackParamList | null>(null)

  useEffect(() => {
    if (!isAuthenticated) {
      setInitialRoute('Login')
      return
    }

    // Check if user has workspaces (admin first login → onboarding)
    workspaceApi.list().then((res) => {
      if (res.data && res.data.length > 0) {
        const ws = res.data[0]
        useWorkspaceStore.getState().setWorkspaces(res.data)
        useWorkspaceStore.getState().setActiveWorkspace(ws)
        setInitialRoute('Workspace')
      } else {
        setInitialRoute('Onboarding')
      }
    }).catch(() => {
      setInitialRoute('Onboarding')
    })
  }, [isAuthenticated])

  if (!initialRoute) return null // loading

  return (
    <NavigationContainer>
      <Stack.Navigator initialRouteName={initialRoute} screenOptions={{ headerShown: false }}>
        {!isAuthenticated ? (
          <>
            <Stack.Screen name="Login" component={LoginScreen} />
            <Stack.Screen name="Invite" component={InviteScreen} />
          </>
        ) : (
          <>
            <Stack.Screen name="Onboarding" component={OnboardingScreen} />
            <Stack.Screen name="Workspace" component={WorkspaceScreen} />
          </>
        )}
      </Stack.Navigator>
    </NavigationContainer>
  )
}
