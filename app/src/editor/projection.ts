/**
 * The projection extractor.
 *
 * The server cannot read a CRDT document, so the client that has it in memory
 * renders it to text and posts that. This is the whole of "each editor supplies
 * only the extraction" — the transport, the table, the route and the rules are
 * the registry's, shared by every editor; this file is the docs-specific part
 * and it is about eighty lines.
 *
 * THE ONE INVARIANT THAT IS A SECURITY PROPERTY: an embed node carries
 * {ref_type, ref_id} and nothing else. No title, no filename, no summary. The
 * document body is an opaque blob the server cannot filter, so the only defence
 * against sharing a document that embeds something the reader may not see is
 * that the body never contains anything worth leaking. `extract` therefore
 * never writes an embed's label into `body_text`, and the refs it emits are
 * bare ids that the server resolves per caller.
 */

/** Bumped whenever a node type is added. The server refuses a projection from
 * a client older than the one that last wrote, because an old extractor
 * silently drops nodes it does not know and would write a lossy body. */
export const SCHEMA_VERSION = 1

export interface OutlineEntry {
  block_id: string
  level: number
  text: string
}

export interface ProjectionRef {
  ref_type: string
  ref_id: string
  block_id?: string
}

export interface Projection {
  seq: number
  schema_version: number
  body_text: string
  outline: OutlineEntry[]
  refs: ProjectionRef[]
}

/** The bounds the server enforces. Applied here too, so a document that is too
 * big produces a truncated projection rather than a rejected one — the server's
 * refusal is the backstop, not the user experience. */
const MAX_BODY_BYTES = 1 << 20
const MAX_OUTLINE = 500
const MAX_REFS = 1000

/** The node types that are references rather than text. */
const EMBED_TYPES: Record<string, string> = {
  driveEmbed: 'file',
  docEmbed: 'file',
  issueEmbed: 'issue',
  mention: 'user',
}

/** A ProseMirror node, as JSON. Deliberately structural: the extractor walks
 * shape rather than importing the schema, so adding a mark cannot break it. */
export interface Node {
  type?: string
  attrs?: Record<string, unknown>
  content?: Node[]
  text?: string
}

/**
 * Renders a document to the projection the server stores.
 *
 * Unknown node types are walked rather than skipped: a block shipped to one
 * client before another knows it must still contribute its text, or the first
 * schema addition makes half the corpus look empty in search.
 */
export function extract(doc: Node, seq: number): Projection {
  const lines: string[] = []
  const outline: OutlineEntry[] = []
  const refs: ProjectionRef[] = []
  const seenRefs = new Set<string>()

  const walk = (node: Node, blockId: string) => {
    const id = typeof node.attrs?.blockId === 'string' ? node.attrs.blockId : blockId

    const embedType = node.type ? EMBED_TYPES[node.type] : undefined
    if (embedType) {
      const refId = node.attrs?.refId ?? node.attrs?.id
      if (typeof refId === 'string' && refId) {
        const key = `${embedType}:${refId}:${id}`
        if (!seenRefs.has(key) && refs.length < MAX_REFS) {
          seenRefs.add(key)
          refs.push({ ref_type: embedType, ref_id: refId, block_id: id.slice(0, 64) })
        }
      }
      // NO TEXT. An embed contributes a reference and nothing else — writing
      // its label here would put the target's name inside a body the server
      // hands to anyone who can read the document.
      return
    }

    if (typeof node.text === 'string') {
      lines.push(node.text)
    }

    if (node.type === 'heading') {
      const level = typeof node.attrs?.level === 'number' ? node.attrs.level : 1
      const text = plainText(node)
      if (text && outline.length < MAX_OUTLINE) {
        outline.push({
          block_id: id.slice(0, 64),
          level: Math.min(6, Math.max(1, level)),
          text: text.slice(0, 512),
        })
      }
    }

    for (const child of node.content ?? []) walk(child, id)

    // A block boundary is a newline, so "the end of one paragraph" and "the
    // start of the next" are not indexed as one word.
    if (isBlock(node)) lines.push('\n')
  }

  walk(doc, '')

  return {
    seq,
    schema_version: SCHEMA_VERSION,
    body_text: clampBytes(lines.join('').replace(/\n{3,}/g, '\n\n').trim(), MAX_BODY_BYTES),
    outline,
    refs,
  }
}

function isBlock(node: Node): boolean {
  switch (node.type) {
    case 'paragraph':
    case 'heading':
    case 'listItem':
    case 'taskItem':
    case 'blockquote':
    case 'codeBlock':
      return true
    default:
      return false
  }
}

function plainText(node: Node): string {
  if (typeof node.text === 'string') return node.text
  return (node.content ?? []).map(plainText).join('')
}

/**
 * Truncates to a BYTE budget, not a character count.
 *
 * The server's limit is octet_length, and one emoji is four bytes. Slicing by
 * characters would send a body the database refuses, which the user would
 * experience as a document that silently stops being searchable.
 */
function clampBytes(text: string, max: number): string {
  const encoder = new TextEncoder()
  if (encoder.encode(text).length <= max) return text
  let lo = 0
  let hi = text.length
  while (lo < hi) {
    const mid = Math.ceil((lo + hi) / 2)
    if (encoder.encode(text.slice(0, mid)).length <= max) lo = mid
    else hi = mid - 1
  }
  return text.slice(0, lo)
}
