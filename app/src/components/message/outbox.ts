import type { Message } from '../../lib/types'
import type { UploadedFile } from '../../api/files'
import type { SendState } from './MessageItem'

/**
 * Client-side outbox for optimistic sends.
 *
 * `Message` has no delivery state — ids are server-assigned, which is exactly
 * why sends were not optimistic and a message vanished for the length of the
 * round trip. A temp id keeps the row in the ascending list until the POST
 * resolves; on failure the row stays put with its text intact so it can be
 * retried instead of being lost behind an Alert.
 */

export interface OutboxEntry {
  status: SendState
  content: string
  files?: UploadedFile[]
  parentId?: string
}

const TEMP_PREFIX = 'temp:'
let seq = 0

export function newTempId(): string {
  return `${TEMP_PREFIX}${Date.now().toString(36)}-${++seq}`
}

/** Server ids are UUIDs, so this prefix cannot collide with one. */
export function isTempId(id: string): boolean {
  return id.startsWith(TEMP_PREFIX)
}

export function optimisticMessage(opts: {
  id: string
  channelId: string
  userId: string
  content: string
  files?: UploadedFile[]
  parentId?: string
}): Message {
  const now = new Date().toISOString()
  return {
    id: opts.id,
    channel_id: opts.channelId,
    user_id: opts.userId,
    parent_id: opts.parentId,
    content: opts.content,
    content_type: 'markdown',
    is_edited: false,
    is_deleted: false,
    reply_count: 0,
    is_pinned: false,
    is_scheduled: false,
    metadata: {},
    reactions: null,
    // The blobs are already uploaded by the time this is built, so the row can
    // render real names and inline previews rather than placeholders.
    files:
      opts.files?.map((f) => ({
        id: f.id,
        name: f.name,
        content_type: f.content_type,
        size_bytes: f.size_bytes,
      })) ?? null,
    created_at: now,
    updated_at: now,
  }
}
