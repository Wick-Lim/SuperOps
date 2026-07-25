import { useEffect, useState } from 'react'
import { workspaceApi } from '../../api/workspaces'
import { useWorkspaceStore } from '../../stores/workspaceStore'
import { useAuthStore } from '../../stores/authStore'
import type { WorkspaceRole } from '../../lib/types'

/**
 * The caller's role in the active workspace.
 *
 * Three screens need it to decide whether to show an admin affordance
 * (`backend/internal/channel/handler.go: canAdministerChannel` accepts a
 * workspace admin as well as a channel admin), and `GET /workspaces/{id}/members`
 * is the only route that reports it. The answer is memoised per workspace so
 * navigating between those screens does not refetch.
 */
const cache = new Map<string, WorkspaceRole | null>()

export function useWorkspaceRole(): { role: WorkspaceRole | null; isAdmin: boolean } {
  const workspaceId = useWorkspaceStore((s) => s.activeWorkspace?.id)
  const userId = useAuthStore((s) => s.user?.id)
  const key = workspaceId && userId ? `${workspaceId}:${userId}` : null

  const [role, setRole] = useState<WorkspaceRole | null>(key ? cache.get(key) ?? null : null)

  useEffect(() => {
    if (!key || !workspaceId || !userId) {
      setRole(null)
      return
    }
    if (cache.has(key)) {
      setRole(cache.get(key) ?? null)
      return
    }
    let cancelled = false
    workspaceApi
      .listMembers(workspaceId)
      .then((res) => {
        const mine = (res.data ?? []).find((m) => m.user_id === userId)?.role ?? null
        cache.set(key, mine)
        if (!cancelled) setRole(mine)
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
