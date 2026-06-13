import { create } from 'zustand'
import type { PublicUser } from '../lib/types'
import { userApi } from '../api/users'

interface UserState {
  users: Record<string, PublicUser>
  pending: Set<string>
  setUser: (u: PublicUser) => void
  ensureUsers: (ids: string[]) => void
}

export const useUserStore = create<UserState>()((set, get) => ({
  users: {},
  pending: new Set(),

  setUser: (u) => set((s) => ({ users: { ...s.users, [u.id]: u } })),

  // Fetch any ids we don't have cached yet (deduped against in-flight requests).
  ensureUsers: (ids) => {
    const { users, pending } = get()
    const missing = ids.filter((id) => id && !users[id] && !pending.has(id))
    if (missing.length === 0) return
    missing.forEach((id) => pending.add(id))
    missing.forEach((id) => {
      userApi.get(id)
        .then((res) => set((s) => ({ users: { ...s.users, [id]: res.data } })))
        .catch(() => {})
        .finally(() => { get().pending.delete(id) })
    })
  },
}))

// Convenience selectors (call with a snapshot of users).
export function displayName(users: Record<string, PublicUser>, id: string): string {
  const u = users[id]
  return u ? (u.full_name || u.username) : id.slice(0, 8)
}
