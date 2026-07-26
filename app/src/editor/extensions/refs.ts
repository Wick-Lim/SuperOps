import { Node, mergeAttributes } from '@tiptap/core'

/**
 * Reference nodes: mentions and embeds.
 *
 * THE ONE RULE, and it is a security property rather than a style choice:
 *
 *     A REFERENCE NODE STORES {refType, refId} AND NOTHING ELSE.
 *
 * No name, no title, no filename, no avatar URL. The document body is an opaque
 * CRDT blob the server cannot filter, so the only defence against sharing a
 * document that references something the reader may not see is that the body
 * NEVER CONTAINS ANYTHING WORTH LEAKING. A label cached in the node would be
 * copied into every export, every channel share and every search projection,
 * and no permission check anywhere could take it back.
 *
 * The label is resolved at RENDER time, per caller, by
 * POST /drive/files/{id}/refs/resolve — which answers `{"access":"denied"}`
 * with no title field at all for a target the reader cannot see. A grey
 * placeholder is the correct UX and also the honest one.
 *
 * The projection extractor asserts this invariant as a test rather than trusting
 * it: see app/test/projection.test.ts, "never writes an embed label into the
 * body".
 */

/** The attribute set every reference node has, and the whole of it. */
function refAttributes() {
  return {
    refId: {
      default: '',
      parseHTML: (el: HTMLElement) => el.getAttribute('data-ref-id') ?? '',
      renderHTML: (attrs: Record<string, unknown>) => ({ 'data-ref-id': String(attrs.refId ?? '') }),
    },
    // The block this reference sits in, so a backlink can say where. Written by
    // the block-id extension; empty is fine.
    blockId: {
      default: '',
      parseHTML: (el: HTMLElement) => el.getAttribute('data-block-id') ?? '',
      renderHTML: (attrs: Record<string, unknown>) =>
        attrs.blockId ? { 'data-block-id': String(attrs.blockId) } : {},
    },
  }
}

/**
 * An @mention of a person.
 *
 * Inline and atomic: it behaves as one character to the cursor, so backspacing
 * removes the whole mention rather than peeling letters off a name the document
 * does not contain.
 */
export const Mention = Node.create({
  name: 'mention',
  group: 'inline',
  inline: true,
  atom: true,
  selectable: true,
  draggable: false,

  addAttributes: refAttributes,

  parseHTML() {
    return [{ tag: 'span[data-mention]' }]
  },

  renderHTML({ HTMLAttributes }) {
    // The rendered text is a PLACEHOLDER, not a name. The NodeView replaces it
    // once the resolver answers; a document rendered without the resolver
    // therefore shows "@…" rather than somebody's name from a stale cache.
    return [
      'span',
      mergeAttributes(HTMLAttributes, { 'data-mention': '', class: 'superops-ref superops-mention' }),
      '@…',
    ]
  },

  renderText() {
    // What the plain-text serializer produces. Deliberately NOT the name: this
    // is what a copy-paste and a markdown export carry.
    return '@'
  },
})

/** A reference to a Drive object — a file, a document, a spreadsheet, a design. */
export const DriveEmbed = Node.create({
  name: 'driveEmbed',
  group: 'block',
  atom: true,
  selectable: true,
  draggable: true,

  addAttributes: refAttributes,

  parseHTML() {
    return [{ tag: 'div[data-drive-embed]' }]
  },

  renderHTML({ HTMLAttributes }) {
    return [
      'div',
      mergeAttributes(HTMLAttributes, {
        'data-drive-embed': '',
        class: 'superops-ref superops-embed',
      }),
      'Attached file',
    ]
  },

  renderText() {
    return ''
  },
})

/** A reference to an issue. */
export const IssueEmbed = Node.create({
  name: 'issueEmbed',
  group: 'inline',
  inline: true,
  atom: true,
  selectable: true,

  addAttributes: refAttributes,

  parseHTML() {
    return [{ tag: 'span[data-issue-embed]' }]
  },

  renderHTML({ HTMLAttributes }) {
    return [
      'span',
      mergeAttributes(HTMLAttributes, {
        'data-issue-embed': '',
        class: 'superops-ref superops-issue',
      }),
      '#…',
    ]
  },

  renderText() {
    return '#'
  },
})

/**
 * The node name → ref_type mapping the extractor uses.
 *
 * Kept beside the nodes so adding one is a single place, and exported so the
 * extractor cannot drift from the schema — a mismatch there would silently stop
 * emitting refs for a node type, and the only symptom would be a backlink list
 * that quietly went empty.
 */
export const REF_TYPES: Record<string, string> = {
  mention: 'user',
  driveEmbed: 'file',
  issueEmbed: 'issue',
}
