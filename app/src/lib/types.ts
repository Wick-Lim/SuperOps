export interface User {
  id: string
  email: string
  username: string
  full_name: string
  avatar_url: string
  is_active: boolean
  status_text?: string
  status_emoji?: string
  created_at: string
}

// PublicUser is what GET /users/{id} and /users/search return.
export interface PublicUser {
  id: string
  username: string
  full_name: string
  avatar_url: string
  is_bot: boolean
}

export interface Workspace {
  id: string
  name: string
  slug: string
  description: string
  icon_url: string
  owner_id: string
  created_at: string
}

export interface WorkspaceMember {
  workspace_id: string
  user_id: string
  role: 'owner' | 'admin' | 'member' | 'guest'
  joined_at: string
}

export interface Channel {
  id: string
  workspace_id: string
  name: string | null
  slug: string | null
  description: string
  type: 'public' | 'private' | 'dm' | 'group_dm'
  topic: string
  is_archived: boolean
  creator_id: string | null
  last_message_at: string | null
  created_at: string
}

export interface ChannelMemberView {
  channel_id: string
  user_id: string
  role: string
  last_read_at: string
  muted: boolean
  notification_pref: string
  joined_at: string
}

export interface Reaction {
  id: string
  message_id: string
  user_id: string
  emoji: string
  created_at: string
}

export interface FileRef {
  id: string
  name: string
  content_type: string
  size_bytes: number
}

export interface Message {
  id: string
  channel_id: string
  user_id: string
  parent_id: string | null
  content: string
  content_type: string
  is_edited: boolean
  is_deleted: boolean
  reply_count: number
  is_pinned?: boolean
  pinned_by?: string | null
  pinned_at?: string | null
  scheduled_at?: string | null
  is_scheduled?: boolean
  reactions?: Reaction[]
  files?: FileRef[]
  created_at: string
  updated_at: string
}

export type NotificationType = 'mention' | 'dm' | 'thread_reply' | 'channel_invite' | 'system'

export interface AppNotification {
  id: string
  user_id: string
  type: NotificationType
  title: string
  body: string
  data: string // JSON string: { channel_id, message_id }
  is_read: boolean
  created_at: string
}

export type PresenceStatus = 'online' | 'away' | 'dnd' | 'offline'

export interface TokenPair {
  access_token: string
  refresh_token: string
  expires_in: number
}

export interface ApiResponse<T> {
  data: T
  meta?: { cursor: string; has_more: boolean }
  error?: { code: string; message: string }
}
