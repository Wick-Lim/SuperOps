import { api, withPaging } from './client'
import type { WorkspaceRole } from '../lib/types'

/** Row of GET /admin/users — a projection, not the full User. */
export interface AdminUser {
  id: string
  email: string
  username: string
  full_name: string
  is_active: boolean
  is_bot: boolean
  created_at: string
}

/** Row of GET /admin/invitations. The token is NEVER returned (only its hash is stored). */
export interface AdminInvitation {
  id: string
  workspace_id: string
  email: string
  role: WorkspaceRole
  status: string
  expires_at: string
  created_at: string
  /** Display name of the inviter. */
  invited_by: string
}

/** Response of POST /admin/invitations — the only time the token is ever visible. */
export interface CreatedInvitation {
  id: string
  /** Relative path containing the plaintext token, e.g. `/invite/<token>`. */
  invite_url: string
  email: string
  role: WorkspaceRole
  workspace_id: string
  expires_at: string
}

export interface AdminStats {
  users: number
  workspaces: number
  channels: number
  messages: number
}

export interface AuditLogEntry {
  id: string
  workspace_id: string | null
  actor_id: string | null
  action: string
  resource_type: string
  resource_id: string | null
  /** JSON text, not an object. */
  metadata: string
  ip_address: string
  created_at: string
}

export const adminApi = {
  /** Users across every workspace the caller administers. Capped at 100. */
  listUsers() {
    return api.get<AdminUser[]>('/admin/users')
  },
  /**
   * `workspace_id` is required whenever `role` is set — a role has no meaning
   * outside a workspace.
   */
  updateUser(
    userId: string,
    data: { is_active?: boolean; role?: Exclude<WorkspaceRole, 'owner'>; workspace_id?: string },
  ) {
    return api.patch<{ message: string }>(`/admin/users/${userId}`, data)
  },
  stats() {
    return api.get<AdminStats>('/admin/stats')
  },
  /** Paginated (`meta.cursor` / `meta.has_more`) — it used to be a bare array. */
  auditLogs(cursor?: string, limit?: number) {
    return api.get<AuditLogEntry[]>(withPaging('/admin/audit-logs', cursor, limit))
  },
  listInvitations() {
    return api.get<AdminInvitation[]>('/admin/invitations')
  },
  /** `workspace_id` is now REQUIRED; the caller must administer that workspace. */
  createInvitation(data: {
    workspace_id: string
    email: string
    role?: Exclude<WorkspaceRole, 'owner'>
  }) {
    return api.post<CreatedInvitation>('/admin/invitations', data)
  },
}
