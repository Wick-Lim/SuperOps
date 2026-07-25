import { api, withPaging } from './client'

/**
 * Comments.
 *
 * ONE surface for every commentable thing. The route carries an OBJECT TYPE and
 * an id, and the server authorizes against that object — so this module works
 * for an issue, a file, a folder or anything else a pillar registers, and it
 * needs no change when one appears.
 */

export interface Comment {
  id: string
  workspace_id: string
  object_type: string
  object_id: string
  author_id: string | null
  parent_id: string | null
  /** Empty for a deleted comment. The row survives because the replies under it
   * still make sense — a thread that silently loses its first message reads as
   * corruption. */
  body: string
  deleted: boolean
  edited_at: string | null
  created_at: string
  updated_at: string
  reply_count: number
}

export const commentApi = {
  list(objectType: string, objectId: string, cursor?: string) {
    return api.get<Comment[]>(withPaging(`/comments/${objectType}/${objectId}`, cursor))
  },

  create(objectType: string, objectId: string, body: string, parentId?: string) {
    return api.post<Comment>(`/comments/${objectType}/${objectId}`, {
      body,
      ...(parentId ? { parent_id: parentId } : {}),
    })
  },

  replies(commentId: string) {
    return api.get<Comment[]>(`/comments/${commentId}/replies`)
  },

  update(commentId: string, body: string) {
    return api.patch<Comment>(`/comments/${commentId}`, { body })
  },

  remove(commentId: string) {
    return api.del<void>(`/comments/${commentId}`)
  },
}

/** MENTION_PATTERN matches the encoded form the server extracts, `<@uuid>`.
 *
 * A bare "@name" is deliberately NOT a mention on either side: names are not
 * unique, they change, and resolving one at read time makes a comment's meaning
 * depend on who is called what today. The composer turns a picked person into
 * this form. */
export const MENTION_PATTERN =
  /<@([0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12})>/g

/** renderMentions splits a body into text and mention segments so a renderer can
 * style the mentions without parsing twice. */
export function renderMentions(
  body: string,
  nameFor: (userId: string) => string,
): Array<{ text: string; mention: boolean }> {
  const out: Array<{ text: string; mention: boolean }> = []
  let last = 0
  for (const m of body.matchAll(MENTION_PATTERN)) {
    const at = m.index ?? 0
    if (at > last) out.push({ text: body.slice(last, at), mention: false })
    out.push({ text: '@' + nameFor(m[1]), mention: true })
    last = at + m[0].length
  }
  if (last < body.length) out.push({ text: body.slice(last), mention: false })
  return out
}
