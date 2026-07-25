import type { Projection } from '../../editor/projection'
import type { DesignModel } from './model'

/**
 * The design surface's extractor. Twenty lines.
 *
 * IT EXTRACTS THE TEXT LAYERS, and nothing else. A canvas's searchable content
 * is its words — the frame names, the labels, the copy in a mock-up — and
 * anything else that could be indexed (colours, coordinates, node counts) is
 * not what anybody types into a search box.
 *
 * The frame names come first because they are the document's structure: they
 * are what a link preview should show and what somebody scanning results
 * recognises.
 */

export const DESIGN_SCHEMA_VERSION = 1

const MAX_LAYERS = 5000

export function extractDesign(model: DesignModel, seq: number): Projection {
  const nodes = model.list()
  const words: string[] = []
  const outline: Projection['outline'] = []

  for (const node of nodes.slice(0, MAX_LAYERS)) {
    const text = node.text.trim()
    if (text === '') continue
    words.push(text)
    // A frame is the canvas's heading. Everything else is body text, and
    // giving a label the same weight as a frame name would fill the outline
    // with button captions.
    if (node.kind === 'frame' && outline.length < 200) {
      outline.push({ block_id: node.id.slice(0, 64), level: 1, text: text.slice(0, 512) })
    }
  }

  return {
    seq,
    schema_version: DESIGN_SCHEMA_VERSION,
    body_text: words.join('\n').slice(0, 1 << 20),
    outline,
    // Present and empty on purpose: the server rebuilds refs wholesale from
    // each projection, so omitting the field would be indistinguishable from
    // "unchanged" and a stale backlink would survive forever.
    refs: [],
  }
}
