import { useEffect, useState } from 'react'
import { workspaceApi } from '../../api/workspaces'
import { useWorkspaceStore } from '../../stores/workspaceStore'
import { useAuthStore } from '../../stores/authStore'
import type { WorkspaceRole } from '../../lib/types'
import { AccountCache } from '../../lib/accountCache'

/**
 * The caller's role in the active workspace.
 *
 * Three screens need it to decide whether to show an admin affordance
 * (`backend/internal/channel/handler.go: canAdministerChannel` accepts a
 * workspace admin as well as a channel admin), and `GET /workspaces/{id}/members`
 * is the only route that reports it. The answer is memoised per workspace so
 * navigating between those screens does not refetch.
 */
const cache = new AccountCache<string, WorkspaceRole | null>()

export function getCachedWorkspaceRole(key: string): WorkspaceRole | null | undefined {
  return cache.get(key)
}

export function clearWorkspaceRoleCache(): void {
  cache.clear()
}

export async function loadWorkspaceRole(
  workspaceId: string,
  userId: string,
): Promise<WorkspaceRole | null | undefined> {
  const key = `${workspaceId}:${userId}`
  if (cache.has(key)) return cache.get(key)
  const generation = cache.generation
  const res = await workspaceApi.listMembers(workspaceId)
  const role = (res.data ?? []).find((member) => member.user_id === userId)?.role ?? null
  return cache.setIfCurrent(generation, key, role) ? role : undefined
}

export function useWorkspaceRole(): { role: WorkspaceRole | null; isAdmin: boolean } {
  const workspaceId = useWorkspaceStore((s) => s.activeWorkspace?.id)
  const userId = useAuthStore((s) => s.user?.id)
  const key = workspaceId && userId ? `${workspaceId}:${userId}` : null

  const [role, setRole] = useState<WorkspaceRole | null>(key ? getCachedWorkspaceRole(key) ?? null : null)

  useEffect(() => {
    if (!key || !workspaceId || !userId) {
      setRole(null)
      return
    }
    let cancelled = false
    loadWorkspaceRole(workspaceId, userId)
      .then((loaded) => {
        if (cancelled || loaded === undefined) return
        setRole(loaded)
      })
      .catch(() => {
        // No answer means no elevated UI — the server is the real gate anyway.
        if (!cancelled) setRole(null)
      })
    return () => {
      cancelled = true
    }
  }, [key, workspaceId, userId])

  return { role, isAdmin: role === 'owner' || role === 'admin' }
}
