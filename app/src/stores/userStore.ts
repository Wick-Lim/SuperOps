import { create } from 'zustand'
import type { PublicUser } from '../lib/types'
import { userApi } from '../api/users'
import { isApiError } from '../lib/errors'

/** Ids retried at most this many times before being parked in `failed`. */
const MAX_ATTEMPTS = 2

interface UserState {
  users: Record<string, PublicUser>
  /** Ids with a GET /users/{id} in flight. */
  pending: Set<string>
  /** Ids that will not be retried until `retryFailed()` is called. */
  failed: Set<string>

  setUser: (u: PublicUser) => void
  setUsers: (list: PublicUser[]) => void
  ensureUsers: (ids: string[]) => void
  /** Clears the failure memo, e.g. after the network comes back. */
  retryFailed: () => void
  clear: () => void
}

/** Immutable Set update — mutating in place is invisible to subscribers. */
function withAdded(set: Set<string>, ids: string[]): Set<string> {
  const next = new Set(set)
  ids.forEach((id) => next.add(id))
  return next
}

function withRemoved(set: Set<string>, id: string): Set<string> {
  if (!set.has(id)) return set
  const next = new Set(set)
  next.delete(id)
  return next
}

// Attempt counts live outside the store: they are pure bookkeeping and must not
// produce a state update (and therefore a rerender) on every retry.
const attempts = new Map<string, number>()

export const useUserStore = create<UserState>()((set, get) => ({
  users: {},
  pending: new Set(),
  failed: new Set(),

  setUser: (u) => set((s) => ({ users: { ...s.users, [u.id]: u } })),

  setUsers: (list) =>
    set((s) => {
      if (list.length === 0) return s
      const users = { ...s.users }
      list.forEach((u) => {
        users[u.id] = u
      })
      return { users }
    }),

  /**
   * Fetch any ids not already cached, in flight, or known-bad.
   *
   * There is no bulk user endpoint, so this is one request per unknown id. The
   * `pending` and `failed` sets are replaced rather than mutated: the previous
   * version called `pending.add(id)` outside `set()`, which never notified a
   * subscriber, and it re-requested permanently-failing ids on every pass.
   */
  ensureUsers: (ids) => {
    const { users, pending, failed } = get()
    const missing = Array.from(
      new Set(ids.filter((id) => id && !users[id] && !pending.has(id) && !failed.has(id))),
    )
    if (missing.length === 0) return

    set({ pending: withAdded(pending, missing) })

    missing.forEach((id) => {
      userApi
        .get(id)
        .then((res) => {
          attempts.delete(id)
          set((s) => ({ users: { ...s.users, [id]: res.data }, pending: withRemoved(s.pending, id) }))
        })
        .catch((err: unknown) => {
          // A 404/403 is permanent for this session; a network blip is not.
          const permanent = isApiError(err) && err.status >= 400 && err.status < 500
          const tries = (attempts.get(id) ?? 0) + 1
          attempts.set(id, tries)
          set((s) => ({
            pending: withRemoved(s.pending, id),
            failed: permanent || tries >= MAX_ATTEMPTS ? withAdded(s.failed, [id]) : s.failed,
          }))
        })
    })
  },

  retryFailed: () => {
    if (get().failed.size === 0) return
    get().failed.forEach((id) => attempts.delete(id))
    set({ failed: new Set() })
  },

  clear: () => {
    attempts.clear()
    set({ users: {}, pending: new Set(), failed: new Set() })
  },
}))

// Convenience selectors (call with a snapshot of users).
export function displayName(users: Record<string, PublicUser>, id: string): string {
  const u = users[id]
  return u ? (u.full_name || u.username) : id.slice(0, 8)
}
