import { api } from './client'
import type { Workspace } from '../lib/types'

export const workspaceApi = {
  list() { return api.get<Workspace[]>('/workspaces') },
  get(id: string) { return api.get<Workspace>(`/workspaces/${id}`) },
  create(data: { name: string; slug: string; description?: string }) {
    return api.post<Workspace>('/workspaces', data)
  },
  update(id: string, data: Partial<Workspace>) {
    return api.patch<Workspace>(`/workspaces/${id}`, data)
  },
}
