import { api } from './client'

export const adminApi = {
  listUsers() {
    return api.get<Array<{ id: string; email: string; username: string; full_name: string; is_active: boolean; created_at: string }>>('/admin/users')
  },
  updateUser(userId: string, data: { is_active?: boolean }) {
    return api.patch<{ message: string }>(`/admin/users/${userId}`, data)
  },
  getStats() {
    return api.get<{ users: number; workspaces: number; channels: number; messages: number }>('/admin/stats')
  },
  getAuditLogs() {
    return api.get<Array<{ id: string; action: string; resource_type: string; actor_id: string; created_at: string }>>('/admin/audit-logs')
  },
}
