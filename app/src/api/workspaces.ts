import { api } from './client'
import type { Workspace, WorkspaceMember, PresenceStatus } from '../lib/types'

export const workspaceApi = {
  list() { return api.get<Workspace[]>('/workspaces') },
  get(id: string) { return api.get<Workspace>(`/workspaces/${id}`) },
  create(data: { name: string; slug: string; description?: string }) {
    return api.post<Workspace>('/workspaces', data)
  },
  update(id: string, data: Partial<Workspace>) {
    return api.patch<Workspace>(`/workspaces/${id}`, data)
  },
  listMembers(id: string) {
    return api.get<WorkspaceMember[]>(`/workspaces/${id}/members`)
  },
  updateMember(id: string, userId: string, role: string) {
    return api.patch<{ message: string }>(`/workspaces/${id}/members/${userId}`, { role })
  },
  removeMember(id: string, userId: string) {
    return api.del<{ message: string }>(`/workspaces/${id}/members/${userId}`)
  },
  presence(id: string) {
    return api.get<Record<string, PresenceStatus>>(`/workspaces/${id}/presence`)
  },
}
