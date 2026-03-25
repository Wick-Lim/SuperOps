export interface User {
  id: string
  email: string
  username: string
  full_name: string
  avatar_url: string
  is_active: boolean
  created_at: string
}

export interface PublicUser {
  id: string
  username: string
  full_name: string
  avatar_url: string
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
  role: string
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

export interface ChannelMember {
  channel_id: string
  user_id: string
  role: string
  last_read_at: string
  muted: boolean
  joined_at: string
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
  created_at: string
  updated_at: string
}

export interface Reaction {
  id: string
  message_id: string
  user_id: string
  emoji: string
  created_at: string
}

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
