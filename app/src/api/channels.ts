import { api } from './client'
import type {
  Channel,
  ChannelMemberView,
  ChannelNotificationPref,
  ChannelUnread,
} from '../lib/types'

export const channelApi = {
  /** Channels the caller is a member of. Only this route populates `unread_count`. */
  list(workspaceId: string) {
    return api.get<Channel[]>(`/workspaces/${workspaceId}/channels`)
  },
  /** Public channels in the workspace, joinable or not. `unread_count` is always 0. */
  browse(workspaceId: string) {
    return api.get<Channel[]>(`/workspaces/${workspaceId}/channels/browse`)
  },
  get(workspaceId: string, channelId: string) {
    return api.get<Channel>(`/workspaces/${workspaceId}/channels/${channelId}`)
  },
  /** `type` must be 'public' or 'private' — anything else is 400 INVALID_CHANNEL_TYPE. */
  create(
    workspaceId: string,
    data: { name: string; slug: string; description?: string; type?: 'public' | 'private' },
  ) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels`, data)
  },
  update(
    workspaceId: string,
    channelId: string,
    data: { name?: string; description?: string; topic?: string },
  ) {
    return api.patch<Channel>(`/workspaces/${workspaceId}/channels/${channelId}`, data)
  },
  /** Idempotent server-side: an existing DM comes back with 200, not a duplicate. */
  createDM(workspaceId: string, userIds: string[]) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/dm`, { user_ids: userIds })
  },
  join(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/join`)
  },
  leave(workspaceId: string, channelId: string) {
    return api.post<{ message: string }>(`/workspaces/${workspaceId}/channels/${channelId}/leave`)
  },
  archive(workspaceId: string, channelId: string) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/${channelId}/archive`)
  },
  unarchive(workspaceId: string, channelId: string) {
    return api.post<Channel>(`/workspaces/${workspaceId}/channels/${channelId}/unarchive`)
  },
  listMembers(workspaceId: string, channelId: string) {
    return api.get<ChannelMemberView[]>(`/workspaces/${workspaceId}/channels/${channelId}/members`)
  },
  /** Adds an existing workspace member. 409 CANNOT_MODIFY_DM on a DM. */
  addMember(
    workspaceId: string,
    channelId: string,
    userId: string,
    role: 'admin' | 'member' = 'member',
  ) {
    return api.post<ChannelMemberView>(
      `/workspaces/${workspaceId}/channels/${channelId}/members`,
      { user_id: userId, role },
    )
  },
  removeMember(workspaceId: string, channelId: string, userId: string) {
    return api.del<{ message: string }>(
      `/workspaces/${workspaceId}/channels/${channelId}/members/${userId}`,
    )
  },
  /**
   * Moves the read marker. With no options it marks everything read as of now;
   * `messageId` marks up to a specific message. Answers the recomputed unread
   * state (it used to answer `{message: "marked as read"}`).
   */
  markRead(channelId: string, opts?: { messageId?: string; readAt?: string }) {
    const body: Record<string, string> = {}
    if (opts?.messageId) body.message_id = opts.messageId
    else if (opts?.readAt) body.read_at = opts.readAt
    return api.put<ChannelUnread>(`/channels/${channelId}/read`, body)
  },
  unread(channelId: string) {
    return api.get<ChannelUnread>(`/channels/${channelId}/unread`)
  },
  /** Per-member mute / notification preference. At least one field is required. */
  updatePrefs(
    channelId: string,
    data: { muted?: boolean; notification_pref?: ChannelNotificationPref },
  ) {
    return api.patch<ChannelMemberView>(`/channels/${channelId}/prefs`, data)
  },
  /** Users the presence service currently believes are typing in the channel. */
  typing(channelId: string) {
    return api.get<{ typing: string[] }>(`/channels/${channelId}/typing`)
  },
}
