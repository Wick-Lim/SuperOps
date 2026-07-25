/** In-memory keystore standing in for expo-secure-store. */

const store = new Map<string, string>()

export async function setItemAsync(key: string, value: string): Promise<void> {
  store.set(key, value)
}

export async function getItemAsync(key: string): Promise<string | null> {
  return store.has(key) ? (store.get(key) as string) : null
}

export async function deleteItemAsync(key: string): Promise<void> {
  store.delete(key)
}

export function __reset(): void {
  store.clear()
}
