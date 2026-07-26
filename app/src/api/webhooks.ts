import { api } from './client'

/** Row of GET /webhooks. The token is never returned after creation. */
export interface Webhook {
  id: string
  workspace_id: string
  name: string
  type: 'incoming'
  channel_id: string
  is_active: boolean
  created_at: string
}

/**
 * Response of POST /webhooks and POST /webhooks/{id}/rotate. `token` is shown
 * exactly once — only its hash is stored, so it cannot be recovered later.
 */
export interface WebhookCredential {
  id: string
  token: string
  /** Always `/api/v1/webhooks/incoming`; authenticate with `Authorization: Bearer <token>`. */
  webhook_url: string
  usage?: string
}

export const webhookApi = {
  /** Webhooks of every workspace the caller administers. */
  list() {
    return api.get<Webhook[]>('/webhooks')
  },
  /** Workspace-admin only. `type` defaults to 'incoming' (the only supported kind). */
  create(data: { name: string; channel_id: string; type?: 'incoming' }) {
    return api.post<WebhookCredential>('/webhooks', data)
  },
  /** Rename and/or enable-disable. At least one field. */
  update(webhookId: string, data: { name?: string; is_active?: boolean }) {
    return api.patch<{ message: string }>(`/webhooks/${webhookId}`, data)
  },
  /**
   * Issues a new token and invalidates the old one.
   *
   * PUT .../token, not POST .../rotate. The server cannot register the latter:
   * it is ambiguous against the legacy delivery route, because
   * "/api/v1/webhooks/incoming/rotate" matches both patterns and neither is
   * more specific, which makes ServeMux panic at registration.
   *
   * This client called the route that does not exist, so rotation 404'd — and
   * rotation is the ONLY remedy for a leaked webhook token. Neither tsc nor
   * the Go compiler can see across this seam; a test now does.
   */
  rotate(webhookId: string) {
    return api.put<WebhookCredential>(`/webhooks/${webhookId}/token`)
  },
  remove(webhookId: string) {
    return api.del<{ message: string }>(`/webhooks/${webhookId}`)
  },
}
