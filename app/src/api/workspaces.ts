import { api } from './client'
import type { Workspace, WorkspaceMember, WorkspaceRole, PresenceStatus } from '../lib/types'

export const workspaceApi = {
  list() {
    return api.get<Workspace[]>('/workspaces')
  },
  /** 404 for a non-member (the API does not confirm the id exists). */
  get(id: string) {
    return api.get<Workspace>(`/workspaces/${id}`)
  },
  create(data: { name: string; slug: string; description?: string }) {
    return api.post<Workspace>('/workspaces', data)
  },
  /**
   * Only these three fields are accepted. Bodies are decoded with
   * DisallowUnknownFields, so the previous `Partial<Workspace>` signature made a
   * type-legal `{slug}` or `{created_at}` a 400.
   */
  update(id: string, data: { name?: string; description?: string; icon_url?: string }) {
    return api.patch<Workspace>(`/workspaces/${id}`, data)
  },
  /** Owner only. Cascades channels, messages and memberships. */
  remove(id: string) {
    return api.del<{ message: string }>(`/workspaces/${id}`)
  },
  listMembers(id: string) {
    return api.get<WorkspaceMember[]>(`/workspaces/${id}/members`)
  },
  /**
   * `role: 'owner'` is rejected with 400 USE_OWNERSHIP_TRANSFER — call
   * `transferOwnership` instead.
   */
  updateMember(id: string, userId: string, role: Exclude<WorkspaceRole, 'owner'>) {
    return api.patch<{ message: string }>(`/workspaces/${id}/members/${userId}`, { role })
  },
  removeMember(id: string, userId: string) {
    return api.del<{ message: string }>(`/workspaces/${id}/members/${userId}`)
  },
  /** Owner only. Atomically demotes the caller and promotes `userId`. */
  transferOwnership(id: string, userId: string) {
    return api.post<{ message: string }>(`/workspaces/${id}/transfer-ownership`, { user_id: userId })
  },
  presence(id: string) {
    return api.get<Record<string, PresenceStatus>>(`/workspaces/${id}/presence`)
  },
}
