import { api } from './client'

/**
 * Huddles — live audio and screen sharing.
 *
 * A DEPLOYMENT-DEPENDENT CAPABILITY (ROADMAP §3c). With no media server
 * configured the routes are not registered at all, so every call here answers
 * 404 — which is why `available()` exists and why the UI asks before it offers
 * a button. A 404 from this module means "this deployment cannot host calls",
 * not "something is broken".
 */

export interface HuddleParticipant {
  huddle_id: string
  participant_sid: string
  user_id: string
  joined_at: string
  left_at: string | null
  is_screen_sharing: boolean
}

export interface Huddle {
  id: string
  workspace_id: string
  scope_type: string
  scope_id: string
  room_name: string
  started_by: string | null
  max_participants: number
  created_at: string
  ended_at: string | null
  ended_reason: string | null
}

export interface ICEServer {
  urls: string[]
  username?: string
  credential?: string
}

export interface HuddleSession {
  huddle: Huddle | null
  /** The join credential. SHORT-LIVED, and it IS the grant — a revoked share
   * does not invalidate one already issued, which is why it expires quickly. */
  token: string
  /** Where the client connects. Empty when media is not deployed. */
  url: string
  /** Whether this caller may speak or share. Read capability is enough to
   * LISTEN — a real product state, not a degraded one — and the server enforces
   * it in the token rather than trusting the client. */
  can_publish: boolean
  ice_servers: ICEServer[] | null
  participants: HuddleParticipant[]
}

export const huddleApi = {
  /** Is there a call in this channel, and may I join it? */
  get(channelId: string) {
    return api.get<HuddleSession | { huddle: null }>(`/channels/${channelId}/huddle`)
  },

  /** Starts one, or joins the one already running. Two people clicking at the
   * same moment land in the SAME room — the server's partial unique index is
   * what guarantees it. */
  start(channelId: string) {
    return api.post<HuddleSession>(`/channels/${channelId}/huddle`)
  },

  /** Ends it for everyone. Needs write capability: hanging up on other people
   * is not a read. */
  end(channelId: string) {
    return api.del<void>(`/channels/${channelId}/huddle`)
  },
}

/**
 * Whether this deployment can host calls at all.
 *
 * Probes the route rather than reading a config flag, because the client has no
 * access to the server's configuration and a hardcoded answer would be wrong on
 * exactly the deployments that matter. A 404 means the routes were never
 * registered.
 */
export async function huddlesAvailable(channelId: string): Promise<boolean> {
  try {
    await huddleApi.get(channelId)
    return true
  } catch (e) {
    const status = (e as { status?: number })?.status
    return status !== 404 && status !== 501
  }
}
