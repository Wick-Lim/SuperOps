import { Platform } from 'react-native'
import AsyncStorage from '@react-native-async-storage/async-storage'
import * as SecureStore from 'expo-secure-store'

/**
 * Credential storage.
 *
 * Tokens go to the platform keystore (iOS Keychain / Android EncryptedSharedPrefs)
 * via expo-secure-store. expo-secure-store has no web implementation, so the web
 * build falls back to AsyncStorage (localStorage) — the same exposure a web app
 * always has, and no worse than before.
 *
 * Every function resolves rather than throws: a storage failure must be
 * observable (it is returned/logged) but must never crash a sign-in.
 */

const useKeystore = Platform.OS === 'ios' || Platform.OS === 'android'

export async function setSecureItem(key: string, value: string): Promise<void> {
  if (useKeystore) {
    await SecureStore.setItemAsync(key, value)
    return
  }
  await AsyncStorage.setItem(key, value)
}

export async function getSecureItem(key: string): Promise<string | null> {
  try {
    if (useKeystore) return await SecureStore.getItemAsync(key)
    return await AsyncStorage.getItem(key)
  } catch {
    // A corrupt keychain entry must read as "signed out", not crash startup.
    return null
  }
}

export async function deleteSecureItem(key: string): Promise<void> {
  try {
    if (useKeystore) {
      await SecureStore.deleteItemAsync(key)
      return
    }
    await AsyncStorage.removeItem(key)
  } catch {
    // Best effort: the in-memory session is already gone.
  }
}
