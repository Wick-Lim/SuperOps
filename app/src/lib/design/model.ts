import * as Y from 'yjs'

/**
 * The design surface's shared state.
 *
 * A flat Y.Map of nodes keyed by id, not a tree. A canvas IS a tree visually —
 * frames contain shapes — but storing it as nested Y.Maps makes reparenting a
 * delete plus an insert, which two clients doing at once turns into a duplicate
 * or a disappearance. A flat map with a `parent` field makes reparenting a
 * single-field write, which is the operation a CRDT resolves correctly by
 * construction.
 *
 * Ordering is the same LexoRank idea the issue board uses, for the same reason:
 * z-order changes constantly and must not renumber siblings.
 */

export type NodeKind = 'frame' | 'rect' | 'ellipse' | 'text' | 'line'

export interface DesignNode {
  id: string
  kind: NodeKind
  /** Empty means the root canvas. */
  parent: string
  /** Sort key among siblings — back to front. Sorted as a plain string, so it
   * must never contain a character that sorts oddly; the generator only ever
   * emits [0-9A-Za-z]. */
  order: string
  x: number
  y: number
  w: number
  h: number
  rotation: number
  fill: string
  stroke: string
  strokeWidth: number
  /** Text content for `text` nodes, and the label of a frame. */
  text: string
  fontSize: number
  opacity: number
}

export const DEFAULTS: Omit<DesignNode, 'id' | 'kind' | 'order'> = {
  parent: '',
  x: 0,
  y: 0,
  w: 160,
  h: 100,
  rotation: 0,
  fill: '#3b82c4',
  stroke: '',
  strokeWidth: 0,
  text: '',
  fontSize: 16,
  opacity: 1,
}

/** Canvas bounds. A design that needs more than this is a design that needs
 * pages, which is a cut. */
export const CANVAS_W = 8000
export const CANVAS_H = 8000
export const MAX_NODES = 5000

const ALPHABET = '0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz'

/**
 * A sort key strictly between two others.
 *
 * The same mechanism as the issue board's rank: reordering one item writes one
 * field, so two people reordering different items never touch the same key.
 * Renumbering siblings — the obvious alternative — makes every reorder a write
 * to every node, which is both a conflict storm and an unbounded CRDT log.
 */
export function orderBetween(lo: string, hi: string): string {
  if (lo === '' && hi === '') return 'U'
  if (lo === '') return before(hi)
  if (hi === '') return after(lo)

  let i = 0
  let prefix = ''
  for (;;) {
    const a = i < lo.length ? ALPHABET.indexOf(lo[i]) : -1
    const b = i < hi.length ? ALPHABET.indexOf(hi[i]) : ALPHABET.length
    if (a === b) {
      prefix += lo[i]
      i++
      continue
    }
    if (b - a > 1) return prefix + ALPHABET[Math.floor((a + b) / 2)]
    // Adjacent digits: keep lo's digit and go deeper, which is what makes the
    // key grow by one character rather than requiring a renumber.
    prefix += i < lo.length ? lo[i] : ALPHABET[0]
    i++
    if (i > 64) return prefix + ALPHABET[Math.floor(ALPHABET.length / 2)]
  }
}

function after(lo: string): string {
  const last = ALPHABET.indexOf(lo[lo.length - 1] ?? '0')
  if (last < ALPHABET.length - 1) return lo.slice(0, -1) + ALPHABET[last + 1]
  return lo + ALPHABET[Math.floor(ALPHABET.length / 2)]
}

function before(hi: string): string {
  const first = ALPHABET.indexOf(hi[0] ?? 'U')
  if (first > 0) return ALPHABET[Math.floor(first / 2)]
  return hi[0] + ALPHABET[Math.floor(ALPHABET.length / 2)]
}

export class DesignModel {
  readonly doc: Y.Doc
  readonly nodes: Y.Map<Y.Map<unknown>>

  constructor(doc: Y.Doc) {
    this.doc = doc
    this.nodes = doc.getMap<Y.Map<unknown>>('nodes')
  }

  /** Every node, back to front, as plain objects. */
  list(): DesignNode[] {
    const out: DesignNode[] = []
    this.nodes.forEach((entry, id) => {
      const node = readNode(id, entry)
      if (node) out.push(node)
    })
    out.sort((a, b) => (a.order < b.order ? -1 : a.order > b.order ? 1 : a.id < b.id ? -1 : 1))
    return out
  }

  get(id: string): DesignNode | null {
    const entry = this.nodes.get(id)
    return entry ? readNode(id, entry) : null
  }

  /** Adds a node. `id` is supplied by the caller so an optimistic selection can
   * name the node it just created without waiting for a round trip. */
  add(id: string, kind: NodeKind, props: Partial<DesignNode>, origin?: unknown): DesignNode | null {
    if (this.nodes.size >= MAX_NODES) return null
    const siblings = this.list().filter((n) => n.parent === (props.parent ?? ''))
    const order = props.order ?? orderBetween(siblings[siblings.length - 1]?.order ?? '', '')
    const node: DesignNode = { ...DEFAULTS, ...props, id, kind, order }
    const entry = new Y.Map<unknown>()
    this.doc.transact(() => {
      for (const [k, v] of Object.entries(node)) {
        if (k === 'id') continue
        entry.set(k, v)
      }
      this.nodes.set(id, entry)
    }, origin)
    return node
  }

  /**
   * Updates fields on one node.
   *
   * FIELD BY FIELD, never by replacing the map. Two people dragging and
   * recolouring the same shape both succeed, because they write different keys;
   * replacing the whole node would make one of them lose the other's change
   * with no way to tell it happened.
   */
  update(id: string, patch: Partial<DesignNode>, origin?: unknown) {
    const entry = this.nodes.get(id)
    if (!entry) return
    this.doc.transact(() => {
      for (const [k, v] of Object.entries(patch)) {
        if (k === 'id' || v === undefined) continue
        entry.set(k, v)
      }
    }, origin)
  }

  remove(id: string, origin?: unknown) {
    this.doc.transact(() => {
      // Children go with the parent. A frame whose children survived it would
      // leave them at the canvas root, which reads as "my shapes moved" rather
      // than "I deleted a frame".
      for (const child of this.list().filter((n) => n.parent === id)) {
        this.nodes.delete(child.id)
      }
      this.nodes.delete(id)
    }, origin)
  }
}

function readNode(id: string, entry: Y.Map<unknown>): DesignNode | null {
  const kind = entry.get('kind')
  if (typeof kind !== 'string') return null
  const num = (k: string, d: number) => {
    const v = entry.get(k)
    return typeof v === 'number' && Number.isFinite(v) ? v : d
  }
  const str = (k: string, d: string) => {
    const v = entry.get(k)
    return typeof v === 'string' ? v : d
  }
  return {
    id,
    kind: kind as NodeKind,
    parent: str('parent', ''),
    order: str('order', 'U'),
    x: clamp(num('x', 0), -CANVAS_W, CANVAS_W),
    y: clamp(num('y', 0), -CANVAS_H, CANVAS_H),
    w: clamp(num('w', 1), 1, CANVAS_W),
    h: clamp(num('h', 1), 1, CANVAS_H),
    rotation: num('rotation', 0),
    fill: str('fill', DEFAULTS.fill),
    stroke: str('stroke', ''),
    strokeWidth: num('strokeWidth', 0),
    text: str('text', ''),
    fontSize: clamp(num('fontSize', 16), 4, 400),
    opacity: clamp(num('opacity', 1), 0, 1),
  }
}

function clamp(v: number, lo: number, hi: number): number {
  return v < lo ? lo : v > hi ? hi : v
}
