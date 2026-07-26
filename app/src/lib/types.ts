/**
 * Hand-written mirror of the Go JSON structs. Every declaration below is
 * checked against the model it mirrors; the file path is named in a comment so
 * the next drift is easy to find.
 *
 * Two Go idioms decide optionality here:
 *  - `*T` with `,omitempty`  → the key is ABSENT (`undefined`), never `null`.
 *    Declared `field?: T`. A `=== null` test on one of these is always false.
 *  - `*T` without `omitempty` → the key is present and may be `null`.
 *    Declared `field: T | null`.
 * Payloads assembled by hand (the scheduled-send worker event) can still emit
 * an explicit `null` for a pointer, so pointer fields are `?: T | null`.
 */

// backend/internal/user/model.go — User
export interface User {
  id: string
  email: string
  username: string
  full_name: string
  avatar_url: string
  status_text: string
  status_emoji: string
  timezone: string
  locale: string
  is_bot: boolean
  is_active: boolean
  /** `*time.Time,omitempty` — absent when the user has never been seen. */
  last_active_at?: string
  created_at: string
  updated_at: string
}

// backend/internal/user/model.go — PublicUser (GET /users/{id}, /users/search)
export interface PublicUser {
  id: string
  username: string
  full_name: string
  avatar_url: string
  status_text: string
  status_emoji: string
  is_bot: boolean
}

// backend/internal/workspace/model.go — Workspace
export interface Workspace {
  id: string
  name: string
  slug: string
  description: string
  icon_url: string
  owner_id: string
  created_at: string
  updated_at: string
}

export type WorkspaceRole = 'owner' | 'admin' | 'member' | 'guest'

// backend/internal/workspace/model.go — Member
export interface WorkspaceMember {
  workspace_id: string
  user_id: string
  role: WorkspaceRole
  joined_at: string
}

export type ChannelType = 'public' | 'private' | 'dm' | 'group_dm'

// backend/internal/channel/model.go — Channel
export interface Channel {
  id: string
  workspace_id: string
  /** `*string` with no omitempty — DMs have no name. */
  name: string | null
  slug: string | null
  description: string
  type: ChannelType
  topic: string
  is_archived: boolean
  creator_id: string | null
  /** `*time.Time,omitempty` — ABSENT on a channel with no messages. */
  last_message_at?: string
  created_at: string
  updated_at: string
  /**
   * Computed per caller. Populated by GET /workspaces/{id}/channels; present
   * but always 0 on /browse, /{channel_id} and the DM-create response.
   */
  unread_count: number
}

export type ChannelNotificationPref = 'all' | 'mentions' | 'none' | 'default'

// backend/internal/channel/model.go — ChannelMember
export interface ChannelMemberView {
  channel_id: string
  user_id: string
  role: 'admin' | 'member'
  last_read_at: string
  muted: boolean
  notification_pref: ChannelNotificationPref
  joined_at: string
}

// backend/internal/channel/model.go — Unread
// Body of GET /channels/{id}/unread and of PUT /channels/{id}/read.
export interface ChannelUnread {
  channel_id: string
  unread_count: number
  last_read_at: string
}

// backend/internal/message/model.go — Reaction
export interface Reaction {
  id: string
  message_id: string
  user_id: string
  emoji: string
  created_at: string
}

// backend/internal/message/model.go — FileRef
export interface FileRef {
  id: string
  name: string
  content_type: string
  size_bytes: number
}

// backend/internal/message/model.go — ForwardRef
export interface ForwardRef {
  message_id: string
  channel_id: string
  user_id: string
  created_at: string
}

// backend/internal/message/model.go — Metadata
export interface MessageMetadata {
  forwarded_from?: ForwardRef
}

// backend/internal/message/model.go — Message
export interface Message {
  id: string
  channel_id: string
  user_id: string
  /** `*string,omitempty`; the scheduled-send worker event emits an explicit null. */
  parent_id?: string | null
  content: string
  content_type: string
  is_edited: boolean
  is_deleted: boolean
  reply_count: number
  is_pinned: boolean
  pinned_by?: string | null
  pinned_at?: string | null
  scheduled_at?: string | null
  is_scheduled: boolean
  metadata: MessageMetadata
  /** `[]*Reaction` with no omitempty — present, but `null` when empty. */
  reactions: Reaction[] | null
  files: FileRef[] | null
  created_at: string
  updated_at: string
}

/**
 * A message as it arrives over the WebSocket. The scheduled-send worker
 * (backend/cmd/worker/main.go) hand-builds its `message.new` payload and omits
 * everything but the seven fields below, so the frame cannot be cast to
 * `Message` — run it through `normalizeMessage` first.
 */
export type WireMessage = Partial<Message> &
  Pick<Message, 'id' | 'channel_id' | 'user_id' | 'content' | 'created_at'>

/** Fills the fields a partial wire payload may omit, so no consumer sees undefined. */
export function normalizeMessage(raw: WireMessage): Message {
  return {
    ...raw,
    content_type: raw.content_type ?? 'text',
    is_edited: raw.is_edited ?? false,
    is_deleted: raw.is_deleted ?? false,
    reply_count: raw.reply_count ?? 0,
    is_pinned: raw.is_pinned ?? false,
    is_scheduled: raw.is_scheduled ?? false,
    metadata: raw.metadata ?? {},
    reactions: raw.reactions ?? null,
    files: raw.files ?? null,
    updated_at: raw.updated_at ?? raw.created_at,
  }
}

// backend/internal/message/model.go — Bookmark (GET /bookmarks)
export interface Bookmark {
  message: Message
  bookmarked_at: string
}

export type NotificationType = 'mention' | 'dm' | 'thread_reply' | 'channel_invite' | 'system'

// backend/internal/notification/model.go — Notification
export interface AppNotification {
  id: string
  user_id: string
  type: NotificationType
  title: string
  body: string
  /** JSON string: `{ channel_id, message_id }`. */
  /** The deep-link payload. An OBJECT from the inbox compat layer, a JSON
   * string from older servers — see parseChannelId, which accepts both. Typed
   * `unknown` rather than one of them so a caller has to decide. */
  data: unknown
  is_read: boolean
  created_at: string
}

export type PresenceStatus = 'online' | 'away' | 'dnd' | 'offline'

// backend/internal/auth/service.go — TokenPair
export interface TokenPair {
  access_token: string
  refresh_token: string
  expires_in: number
}

/**
 * backend/pkg/httputil/response.go — Response.
 * `meta` and `error` are `omitempty`: on success there is NO `error` key at all
 * (it is `undefined`, not `null`).
 */
export interface ApiResponse<T> {
  data: T
  meta?: { cursor?: string; has_more: boolean }
  error?: { code: string; message: string; details?: unknown }
}
