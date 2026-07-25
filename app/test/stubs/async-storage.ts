/** In-memory stand-in for @react-native-async-storage/async-storage. */

const store = new Map<string, string>()

const AsyncStorage = {
  async getItem(key: string): Promise<string | null> {
    return store.has(key) ? (store.get(key) as string) : null
  },
  async setItem(key: string, value: string): Promise<void> {
    store.set(key, value)
  },
  async removeItem(key: string): Promise<void> {
    store.delete(key)
  },
  async clear(): Promise<void> {
    store.clear()
  },
}

export default AsyncStorage
