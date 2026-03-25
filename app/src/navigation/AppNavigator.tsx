import React from 'react'
import { NavigationContainer } from '@react-navigation/native'
import { createNativeStackNavigator } from '@react-navigation/native-stack'
import { useAuthStore } from '../stores/authStore'
import LoginScreen from '../screens/LoginScreen'
import RegisterScreen from '../screens/RegisterScreen'
import SetupScreen from '../screens/SetupScreen'
import WorkspaceScreen from '../screens/WorkspaceScreen'

export type RootStackParamList = {
  Login: undefined
  Register: undefined
  Setup: undefined
  Workspace: { workspaceId: string }
}

const Stack = createNativeStackNavigator<RootStackParamList>()

export default function AppNavigator() {
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated)

  return (
    <NavigationContainer>
      <Stack.Navigator screenOptions={{ headerShown: false }}>
        {!isAuthenticated ? (
          <>
            <Stack.Screen name="Login" component={LoginScreen} />
            <Stack.Screen name="Register" component={RegisterScreen} />
          </>
        ) : (
          <>
            <Stack.Screen name="Setup" component={SetupScreen} />
            <Stack.Screen name="Workspace" component={WorkspaceScreen} />
          </>
        )}
      </Stack.Navigator>
    </NavigationContainer>
  )
}
