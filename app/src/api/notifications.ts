import { api, withPaging } from './client'
import type { AppNotification } from '../lib/types'

export type InboxState = 'unread' | 'read' | 'done'

export interface InboxItem {
  id: string
  user_id: string
  workspace_id: string
  subject_type: string
  subject_id: string
  state: InboxState
  top_kind: string
  unread_count: number
  event_count: number
  last_event_id: string | null
  last_actor_id: string | null
  title: string
  preview: string
  first_at: string
  last_at: string
  read_at: string | null
  done_at: string | null
  notified_at: string | null
}

export interface InboxEvent {
  id: string
  user_id: string
  workspace_id: string
  kind: string
  object_type: string
  object_id: string
  subject_type: string
  subject_id: string
  actor_id: string | null
  title: string
  body: string
  data: Record<string, unknown>
  created_at: string
}

export interface NotificationPrefRow {
  kind: string
  in_app: boolean
  push: boolean
  email: 'never' | 'digest' | 'immediate'
}

export interface NotificationPrefsResponse {
  workspace_id: string
  prefs: NotificationPrefRow[]
  default?: NotificationPrefRow
  note?: string
}

export const notificationApi = {
  /** Legacy compatibility surface (backed by inbox_items on the server). */
  list(cursor?: string, limit?: number) {
    return api.get<AppNotification[]>(withPaging('/notifications', cursor, limit))
  },
  markRead(id: string) {
    return api.put<{ message?: string; state?: string }>(`/inbox/${id}/read`)
  },
  markAllRead(opts?: { workspace_id?: string; kind?: string }) {
    return api.post<{ updated: number }>('/inbox/read-all', opts ?? {})
  },
  unreadCount() {
    return api.get<{ count: number }>('/notifications/unread-count')
  },

  /** Unified Inbox surface (/api/v1/inbox & /api/v1/notification-prefs). */
  inbox(opts?: {
    state?: InboxState
    workspace_id?: string
    kind?: string
    cursor?: string
    limit?: number
  }) {
    const params = new URLSearchParams()
    if (opts?.state) params.set('state', opts.state)
    if (opts?.workspace_id) params.set('workspace_id', opts.workspace_id)
    if (opts?.kind) params.set('kind', opts.kind)
    const base = params.toString() ? `/inbox?${params.toString()}` : '/inbox'
    return api.get<InboxItem[]>(withPaging(base, opts?.cursor, opts?.limit))
  },
  inboxCount() {
    return api.get<{ unread: number; by_workspace: { workspace_id: string; unread: number }[] }>(
      '/inbox/count',
    )
  },
  inboxEvents(itemId: string, cursor?: string, limit?: number) {
    return api.get<InboxEvent[]>(withPaging(`/inbox/${itemId}/events`, cursor, limit))
  },
  markUnread(id: string) {
    return api.put<{ state: string }>(`/inbox/${id}/unread`)
  },
  markDone(id: string) {
    return api.put<{ state: string }>(`/inbox/${id}/done`)
  },
  markUndone(id: string) {
    return api.put<{ state: string }>(`/inbox/${id}/undone`)
  },
  listPrefs(workspaceId: string) {
    return api.get<NotificationPrefsResponse>(
      `/notification-prefs?workspace_id=${encodeURIComponent(workspaceId)}`,
    )
  },
  replacePrefs(workspaceId: string, prefs: NotificationPrefRow[]) {
    return api.put<NotificationPrefsResponse>('/notification-prefs', {
      workspace_id: workspaceId,
      prefs,
    })
  },
}
