import { api, withPaging } from './client'

/**
 * The shared inbox.
 *
 * PERMISSIONS ARE THE MAILBOX'S. A conversation inherits from the mailbox it
 * belongs to, so there is no per-thread sharing UI and no per-thread capability
 * to render — an agent is granted once and every thread, past and future,
 * follows. That is why the conversation list is authorized on the mailbox and
 * not filtered per row.
 */

export interface Mailbox {
  id: string
  workspace_id: string
  address: string
  display_name: string
  prefix: string
  archived_at: string | null
  created_at: string
  auto_reply_enabled: boolean
}

export type ConversationState = 'open' | 'pending' | 'closed' | 'spam'
export type ConversationPriority = 'low' | 'normal' | 'high' | 'urgent'

export interface Conversation {
  id: string
  mailbox_id: string
  workspace_id: string
  number: number
  /** "SUP-14" — what the customer sees, built from the mailbox prefix. */
  reference: string
  subject: string
  state: ConversationState
  priority: ConversationPriority
  assignee_id: string | null
  customer_email: string
  customer_name: string
  last_message_at: string
  /** True when the last message was inbound: the ball is with us. */
  awaiting_reply: boolean
  created_at: string
}

export interface MailMessage {
  id: string
  conversation_id: string
  direction: 'inbound' | 'outbound'
  message_id: string
  in_reply_to: string
  from_address: string
  from_name: string
  to_addresses: string[]
  cc_addresses: string[]
  subject: string
  body_text: string
  body_html: string
  author_id: string | null
  /**
   * NULL on an outbound message means WRITTEN, NOT YET DELIVERED — not
   * "unknown". The thread renders that state explicitly, because a reply that
   * is sitting undelivered looks identical to one that was sent unless
   * somebody says so.
   */
  sent_at: string | null
  created_at: string
  /** Drive files owned by this message. Always an array — the server sends []
   * rather than null, because a client doing `.map()` on a null would throw on
   * every message without an attachment, which is most of them. */
  attachments: MailAttachment[]
}

export interface MailAttachment {
  file_id: string
  name: string
  content_type: string
  size_bytes: number
}

export interface MailDomain {
  id: string
  domain: string
  verified_at: string | null
  created_at: string
  /** Where the operator publishes the TXT record, and what to publish. */
  verify_host: string
  verify_value: string
}

export interface IngestToken {
  id: string
  name: string
  created_at: string
  revoked_at: string | null
  /** Present ONLY in the create response. */
  token?: string
}

export const mailboxApi = {
  list(workspaceId: string) {
    return api.get<Mailbox[]>(`/workspaces/${workspaceId}/mailboxes`)
  },

  create(workspaceId: string, address: string, displayName: string, prefix: string) {
    return api.post<Mailbox>(`/workspaces/${workspaceId}/mailboxes`, {
      address,
      display_name: displayName,
      prefix,
    })
  },

  conversations(mailboxId: string, state?: string, cursor?: string) {
    const base = `/mailboxes/${mailboxId}/conversations${state ? `?state=${state}` : ''}`
    return api.get<Conversation[]>(withPaging(base, cursor))
  },

  conversation(conversationId: string) {
    return api.get<{ conversation: Conversation; messages: MailMessage[] }>(
      `/conversations/${conversationId}`,
    )
  },

  patch(
    conversationId: string,
    changes: { state?: ConversationState; priority?: ConversationPriority; assignee_id?: string },
  ) {
    return api.patch<Conversation>(`/conversations/${conversationId}`, changes)
  },

  reply(conversationId: string, bodyText: string, subject?: string, attachmentFileIds?: string[]) {
    return api.post<MailMessage>(`/conversations/${conversationId}/reply`, {
      body_text: bodyText,
      subject,
      // File IDS, not bytes: an agent attaching something has already uploaded
      // it, and re-uploading would make a second copy counting against the
      // quota twice.
      attachment_file_ids: attachmentFileIds,
    })
  },

  // --- administration ------------------------------------------------------

  domains(workspaceId: string) {
    return api.get<MailDomain[]>(`/workspaces/${workspaceId}/mail/domains`)
  },

  createDomain(workspaceId: string, domain: string) {
    return api.post<MailDomain>(`/workspaces/${workspaceId}/mail/domains`, { domain })
  },

  /** Answers `{verified,reason}` rather than failing: an unpublished record is
   * the normal first attempt, not an error. */
  verifyDomain(domainId: string) {
    return api.post<{ verified: boolean; reason?: string }>(`/mail/domains/${domainId}/verify`)
  },

  ingestTokens(workspaceId: string) {
    return api.get<IngestToken[]>(`/workspaces/${workspaceId}/mail/ingest-tokens`)
  },

  createIngestToken(workspaceId: string, name: string) {
    return api.post<{ token: IngestToken; note: string }>(
      `/workspaces/${workspaceId}/mail/ingest-tokens`,
      { name },
    )
  },

  revokeIngestToken(tokenId: string) {
    return api.del<void>(`/mail/ingest-tokens/${tokenId}`)
  },
}
