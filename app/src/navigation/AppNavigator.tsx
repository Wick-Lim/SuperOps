import React, { useCallback, useEffect, useState } from 'react'
import { ActivityIndicator, Pressable, Text, View } from 'react-native'
import { NavigationContainer, type LinkingOptions } from '@react-navigation/native'
import {
  createNativeStackNavigator,
  type NativeStackScreenProps,
} from '@react-navigation/native-stack'
import { useAuthStore } from '../stores/authStore'
import { workspaceApi } from '../api/workspaces'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { errorMessage, isApiError } from '../api/client'
import { usePushNotifications } from '../lib/push'
import { navigationRef } from './navigationRef'
import { API_BASE_URL } from '../config'
import { theme } from '../lib/theme'
import { useResponsive } from '../lib/responsive'
import LoginScreen from '../screens/LoginScreen'
import InviteScreen from '../screens/InviteScreen'
import OnboardingScreen from '../screens/OnboardingScreen'
import WorkspaceScreen from '../screens/WorkspaceScreen'
import SearchScreen from '../screens/SearchScreen'
import NotificationsScreen from '../screens/NotificationsScreen'
import NewChannelScreen from '../screens/NewChannelScreen'
import NewDMScreen from '../screens/NewDMScreen'
import MembersScreen from '../screens/MembersScreen'
import SettingsScreen from '../screens/SettingsScreen'
import AdminScreen from '../screens/AdminScreen'
import ChannelDetailScreen from '../screens/ChannelDetailScreen'
import PinnedMessagesScreen from '../screens/PinnedMessagesScreen'
import ScheduledMessagesScreen from '../screens/ScheduledMessagesScreen'
import BookmarksScreen from '../screens/BookmarksScreen'

export type RootStackParamList = {
  Login: undefined
  /** From `invite_url` (`/invite/<token>`); the token only ever arrives in a URL. */
  Invite: { token?: string } | undefined
  Onboarding: undefined
  Workspace: { workspaceId?: string } | undefined
  Search: undefined
  Notifications: undefined
  NewChannel: undefined
  NewDM: undefined
  Members: { channelId: string }
  Settings: undefined
  Admin: undefined
  ChannelDetail: { channelId: string }
  Pins: { channelId: string }
  Scheduled: { channelId: string }
  Bookmarks: undefined
  /** Internal: shown while the post-login workspace lookup is in flight. */
  Bootstrap: undefined
}

/** Screen props for any route — the typed alternative to `{ navigation: any }`. */
export type RootStackScreenProps<T extends keyof RootStackParamList> = NativeStackScreenProps<
  RootStackParamList,
  T
>

/**
 * Types the untyped hooks (`useNavigation()`, `useRoute()`, `useLinkTo()`)
 * app-wide. Most screens are still declared `{ navigation: any }`, and this is
 * what makes their `navigate('Members', { channelId })` calls checked anyway:
 * route names and param shapes now come from this table rather than `any`.
 */
declare global {
  // eslint-disable-next-line @typescript-eslint/no-namespace
  namespace ReactNavigation {
    // eslint-disable-next-line @typescript-eslint/no-empty-object-type
    interface RootParamList extends RootStackParamList {}
  }
}

const Stack = createNativeStackNavigator<RootStackParamList>()

/** Native deep links need `"scheme": "superops"` in app.json. */
const APP_SCHEME = 'superops'

function linkingPrefixes(): string[] {
  const prefixes = [`${APP_SCHEME}://`]
  // Web: whatever origin the bundle is served from.
  if (typeof window !== 'undefined' && window.location?.origin) {
    prefixes.push(window.location.origin)
  }
  // Universal / app links: production serves the app behind the API's ingress.
  const apiOrigin = API_BASE_URL.replace(/\/api\/v1\/?$/, '')
  if (/^https?:\/\//i.test(apiOrigin) && !prefixes.includes(apiOrigin)) prefixes.push(apiOrigin)
  return prefixes
}

/**
 * Without this the `Invite: { token?: string }` param was unreachable: the only
 * way in was LoginScreen's "have an invite?" link, which navigates with `{}` and
 * leaves the user to paste the token by hand.
 */
const linking: LinkingOptions<RootStackParamList> = {
  prefixes: linkingPrefixes(),
  config: {
    screens: {
      Login: 'login',
      // Mirrors the backend's invite_url exactly: /invite/<token>.
      Invite: 'invite/:token?',
      Onboarding: 'onboarding',
      Workspace: 'workspace/:workspaceId?',
      Search: 'search',
      Notifications: 'notifications',
      NewChannel: 'channels/new',
      NewDM: 'dm/new',
      Members: 'channels/:channelId/members',
      ChannelDetail: 'channels/:channelId',
      Pins: 'channels/:channelId/pins',
      Scheduled: 'channels/:channelId/scheduled',
      Bookmarks: 'bookmarks',
      Settings: 'settings',
      Admin: 'admin',
    },
  },
}

type Boot =
  | { status: 'loading' }
  | { status: 'error'; message: string }
  | { status: 'ready'; route: 'Workspace' | 'Onboarding' }

function BootstrapView({ boot, onRetry }: { boot: Boot; onRetry: () => void }) {
  // The only pre-shell screen that renders prose. Uncapped, its error message
  // ran the full width of the window; `maxWidth` is the whole fix, and the
  // pointer tiers tighten the buttons with everything else.
  const { minTouch } = useResponsive()
  return (
    <View
      style={{
        flex: 1,
        backgroundColor: theme.bg,
        alignItems: 'center',
        justifyContent: 'center',
        padding: 32,
        gap: 16,
        maxWidth: 480,
        alignSelf: 'center',
        width: '100%',
      }}
    >
      {boot.status === 'error' ? (
        <>
          <Text
            accessibilityRole="header"
            style={{ color: theme.text, fontSize: 17, fontWeight: '700', textAlign: 'center' }}
          >
            Could not load your workspaces
          </Text>
          <Text style={{ color: theme.textMuted, textAlign: 'center' }}>{boot.message}</Text>
          <Pressable
            onPress={onRetry}
            accessibilityRole="button"
            accessibilityLabel="Try again"
            style={{
              backgroundColor: theme.primary,
              borderRadius: 12,
              paddingHorizontal: 24,
              minHeight: minTouch,
              justifyContent: 'center',
            }}
          >
            <Text style={{ color: theme.primaryText, fontWeight: '600' }}>Try again</Text>
          </Pressable>
          {/* Otherwise this screen is a dead end: it is the only thing rendered
              while authenticated-but-unbootstrapped, so there is no Settings to
              sign out from. */}
          <Pressable
            onPress={() => {
              void useAuthStore.getState().logout()
            }}
            accessibilityRole="button"
            accessibilityLabel="Sign out"
            style={{ paddingHorizontal: 24, minHeight: minTouch, justifyContent: 'center' }}
          >
            <Text style={{ color: theme.textMuted, fontWeight: '600' }}>Sign out</Text>
          </Pressable>
        </>
      ) : (
        <>
          <ActivityIndicator size="large" color={theme.primary} />
          <Text accessibilityLiveRegion="polite" style={{ color: theme.textMuted, fontSize: 14 }}>
            Loading your workspaces…
          </Text>
        </>
      )}
    </View>
  )
}

export default function AppNavigator() {
  const isAuthenticated = useAuthStore((s) => s.isAuthenticated)
  const [boot, setBoot] = useState<Boot>({ status: 'loading' })
  const [attempt, setAttempt] = useState(0)

  useEffect(() => {
    if (!isAuthenticated) {
      // Reset, so the next sign-in re-runs the lookup instead of reusing the
      // previous account's answer.
      setBoot({ status: 'loading' })
      return
    }

    let cancelled = false
    setBoot({ status: 'loading' })

    workspaceApi
      .list()
      .then((res) => {
        if (cancelled) return
        const list = res.data ?? []
        const store = useWorkspaceStore.getState()
        store.setWorkspaces(list)
        if (list.length === 0) {
          // `setActiveWorkspace` is typed `(ws: Workspace) => void` and cannot
          // clear, but a previous account's workspace must not survive into an
          // empty one — every screen reads `activeWorkspace.id` unguarded.
          useWorkspaceStore.setState({ activeWorkspace: null })
          setBoot({ status: 'ready', route: 'Onboarding' })
          return
        }
        const active = store.activeWorkspace
        store.setActiveWorkspace(list.find((w) => w.id === active?.id) ?? list[0])
        setBoot({ status: 'ready', route: 'Workspace' })
      })
      .catch((err: unknown) => {
        if (cancelled) return
        // An expired session has already cleared the auth store; that state
        // change routes to Login on its own, and an error screen would flash
        // over the top of it.
        if (isApiError(err) && err.isAuthExpired) return
        // Sending every failure — including a dropped connection — to the
        // onboarding wizard offered an existing user a second workspace.
        setBoot({ status: 'error', message: errorMessage(err, 'Could not reach the server.') })
      })

    // Without this, a logout during the in-flight request still wrote state
    // afterwards and dropped the signed-out user into the app stack.
    return () => {
      cancelled = true
    }
  }, [isAuthenticated, attempt])

  const retry = useCallback(() => setAttempt((n) => n + 1), [])

  /**
   * The post-login routing fix.
   *
   * A navigator's router is built exactly once (`useLazyValue` in
   * @react-navigation/core), which freezes `initialRouteName` at mount. When the
   * screen set swapped on login the router therefore fell back to
   * `routeNames[0]` — Onboarding — and every returning user was walked through
   * the workspace wizard again; the later `setInitialRoute('Workspace')` was
   * read by nothing.
   *
   * Two things fix it: `phase` as the navigator key, so a screen-set change
   * rebuilds the router with the matching `initialRouteName`; and declaring the
   * landing screen FIRST in each phase, so even the `routeNames[0]` fallback
   * lands in the right place.
   */
  const phase: 'auth' | 'bootstrap' | 'onboarding' | 'app' = !isAuthenticated
    ? 'auth'
    : boot.status !== 'ready'
      ? 'bootstrap'
      : boot.route === 'Onboarding'
        ? 'onboarding'
        : 'app'

  // Push registration is deliberately gated on `phase === 'app'`, not on
  // `isAuthenticated`. The permission prompt is one-shot on iOS, so asking
  // before the user has a workspace — i.e. before they have seen the product at
  // all — spends it on the moment people are most likely to decline.
  usePushNotifications(phase === 'app')

  const initialRouteName: keyof RootStackParamList =
    phase === 'auth'
      ? 'Login'
      : phase === 'bootstrap'
        ? 'Bootstrap'
        : phase === 'onboarding'
          ? 'Onboarding'
          : 'Workspace'

  // OnboardingScreen finishes with `navigation.replace('Workspace')`, so the
  // main screens have to stay reachable during onboarding — only the order
  // changes.
  const onboarding = <Stack.Screen name="Onboarding" component={OnboardingScreen} />
  const main = (
    <>
      <Stack.Screen name="Workspace" component={WorkspaceScreen} />
      <Stack.Screen name="Search" component={SearchScreen} />
      <Stack.Screen name="Notifications" component={NotificationsScreen} />
      <Stack.Screen name="NewChannel" component={NewChannelScreen} />
      <Stack.Screen name="NewDM" component={NewDMScreen} />
      <Stack.Screen name="Members" component={MembersScreen} />
      <Stack.Screen name="ChannelDetail" component={ChannelDetailScreen} />
      <Stack.Screen name="Pins" component={PinnedMessagesScreen} />
      <Stack.Screen name="Scheduled" component={ScheduledMessagesScreen} />
      <Stack.Screen name="Bookmarks" component={BookmarksScreen} />
      <Stack.Screen name="Settings" component={SettingsScreen} />
      <Stack.Screen name="Admin" component={AdminScreen} />
    </>
  )

  return (
    // The ref is what lets a notification tap navigate: the response can arrive
    // before any screen has mounted (cold start), so there is no component in
    // scope to call useNavigation() from.
    <NavigationContainer
      ref={navigationRef}
      linking={linking}
      fallback={<BootstrapView boot={boot} onRetry={retry} />}
    >
      <Stack.Navigator
        key={phase}
        initialRouteName={initialRouteName}
        screenOptions={{ headerShown: false }}
      >
        {phase === 'auth' ? (
          <>
            <Stack.Screen name="Login" component={LoginScreen} />
            <Stack.Screen name="Invite" component={InviteScreen} />
          </>
        ) : phase === 'bootstrap' ? (
          // Rendering `null` here showed a blank screen on every cold start with
          // a stored token — indefinitely if the request hung.
          <Stack.Screen name="Bootstrap">
            {() => <BootstrapView boot={boot} onRetry={retry} />}
          </Stack.Screen>
        ) : phase === 'onboarding' ? (
          <>
            {onboarding}
            {main}
          </>
        ) : (
          <>
            {main}
            {onboarding}
          </>
        )}
      </Stack.Navigator>
    </NavigationContainer>
  )
}
