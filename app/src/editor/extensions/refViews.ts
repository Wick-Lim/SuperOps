import { Plugin, PluginKey } from '@tiptap/pm/state'
import { Decoration, DecorationSet } from '@tiptap/pm/view'
import type { EditorState } from '@tiptap/pm/state'
import { Extension } from '@tiptap/core'
import { REF_TYPES } from './refs'
import { deniedLabel, type RefResolver } from '../refResolver'

/**
 * Rendering a reference's label without putting it in the document.
 *
 * The node stores {refType, refId}; the label is fetched per caller and painted
 * on top as a DECORATION. That distinction is the whole design: a decoration is
 * a view-layer artefact, so the resolved name never enters the document state,
 * never reaches the CRDT, never reaches the projection, and never survives a
 * copy or an export. A NodeView that wrote the label into an attribute would
 * put it in all four.
 *
 * It also means the same document renders differently for two readers, which is
 * correct — they can see different things — and impossible if the label lived
 * in the shared state.
 */

const key = new PluginKey('superops-ref-labels')

interface Cell {
  label: string
  denied: boolean
}

export function RefLabels(resolver: RefResolver) {
  // Resolved labels, keyed by "type:id". Kept outside the plugin state because
  // it is a cache of an async answer, not editor state — putting it in a
  // transaction would make every resolution an undoable step.
  const labels = new Map<string, Cell>()
  let redraw: (() => void) | null = null

  return Extension.create({
    name: 'superopsRefLabels',

    addProseMirrorPlugins() {
      return [
        new Plugin({
          key,
          view(view) {
            redraw = () => {
              // An empty transaction, purely to make the plugin recompute its
              // decorations. setMeta marks it so nothing mistakes it for a
              // document change.
              view.dispatch(view.state.tr.setMeta(key, true))
            }
            return {
              destroy() {
                redraw = null
              },
            }
          },
          props: {
            decorations(state: EditorState) {
              const decorations: Decoration[] = []
              state.doc.descendants((node, pos) => {
                const refType = REF_TYPES[node.type.name]
                if (!refType) return
                const refId = String(node.attrs.refId ?? '')
                if (!refId) return

                const cacheKey = `${refType}:${refId}`
                const known = labels.get(cacheKey)
                if (!known) {
                  // Ask once. The resolver batches everything requested in the
                  // same frame into one authorized round trip.
                  void resolver.resolve(refType, refId).then((answer) => {
                    labels.set(cacheKey, {
                      label: answer.denied ? deniedLabel(refType) : answer.title ?? '',
                      denied: answer.denied,
                    })
                    redraw?.()
                  })
                  return
                }

                decorations.push(
                  Decoration.node(pos, pos + node.nodeSize, {
                    // The label rides in a data attribute and is painted by CSS
                    // content, so it is unselectable and uncopyable — a reader
                    // cannot paste a name out of a document that does not
                    // contain it.
                    'data-label': known.label,
                    class: known.denied ? 'superops-ref-denied' : 'superops-ref-resolved',
                  }),
                )
              })
              return DecorationSet.create(state.doc, decorations)
            },
          },
        }),
      ]
    },
  })
}
