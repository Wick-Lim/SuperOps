import { createNavigationContainerRef } from '@react-navigation/native'
import type { RootStackParamList } from './AppNavigator'

/**
 * Navigation handle for code that runs outside the React tree.
 *
 * A notification tap is exactly that: the OS hands the response to a listener
 * that may fire before any screen has mounted (cold start) or while the app is
 * in the background, so there is no component in scope to call
 * `useNavigation()` from.
 *
 * The import of `RootStackParamList` is type-only, so this module does not
 * create a runtime cycle with AppNavigator — which imports it back.
 */
export const navigationRef = createNavigationContainerRef<RootStackParamList>()
