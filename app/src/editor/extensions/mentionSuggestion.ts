import { Extension } from '@tiptap/core'
import Suggestion from '@tiptap/suggestion'
import { userApi } from '../../api/users'

/**
 * The `@` menu.
 *
 * WHAT IT INSERTS IS A BARE REFERENCE. The picker shows names — it has to, or
 * nobody could choose — but what lands in the document is {refType:'user',
 * refId:<uuid>}. The name the author saw is discarded at the moment of
 * insertion, because keeping it would put a person's name into a document body
 * that gets copied, exported and indexed with no permission check attached.
 *
 * That also makes the mention correct over time: somebody who changes their
 * display name changes it everywhere, including in documents written before
 * they did. A cached label would freeze whoever they used to be.
 */

interface Candidate {
  id: string
  label: string
}

/** The picker's rendering. Deliberately plain DOM: it is a transient popup, it
 * lives for the length of one keystroke sequence, and mounting a React tree per
 * character would be more machinery than the thing deserves. */
function renderPicker() {
  let el: HTMLDivElement | null = null
  let items: Candidate[] = []
  let selected = 0
  let onPick: ((item: Candidate) => void) | null = null

  const paint = () => {
    if (!el) return
    el.innerHTML = ''
    if (items.length === 0) {
      const empty = document.createElement('div')
      empty.className = 'superops-mention-empty'
      empty.textContent = 'No one matches'
      el.appendChild(empty)
      return
    }
    items.forEach((item, i) => {
      const row = document.createElement('button')
      row.type = 'button'
      row.className = 'superops-mention-item' + (i === selected ? ' is-selected' : '')
      row.textContent = item.label
      row.addEventListener('mousedown', (e) => {
        // mousedown, not click: the editor loses focus on mouseup and the
        // suggestion would be torn down before a click ever fired.
        e.preventDefault()
        onPick?.(item)
      })
      el!.appendChild(row)
    })
  }

  return {
    onStart(props: { items: Candidate[]; command: (item: Candidate) => void; clientRect?: (() => DOMRect | null) | null }) {
      items = props.items
      selected = 0
      onPick = props.command
      el = document.createElement('div')
      el.className = 'superops-mention-menu'
      document.body.appendChild(el)
      const rect = props.clientRect?.()
      if (rect) {
        el.style.position = 'absolute'
        el.style.left = `${rect.left + window.scrollX}px`
        el.style.top = `${rect.bottom + window.scrollY + 4}px`
      }
      paint()
    },
    onUpdate(props: { items: Candidate[]; command: (item: Candidate) => void }) {
      items = props.items
      onPick = props.command
      selected = Math.min(selected, Math.max(0, items.length - 1))
      paint()
    },
    onKeyDown(props: { event: KeyboardEvent }) {
      if (items.length === 0) return false
      switch (props.event.key) {
        case 'ArrowDown':
          selected = (selected + 1) % items.length
          paint()
          return true
        case 'ArrowUp':
          selected = (selected - 1 + items.length) % items.length
          paint()
          return true
        case 'Enter':
          onPick?.(items[selected])
          return true
        case 'Escape':
          return true
        default:
          return false
      }
    },
    onExit() {
      el?.remove()
      el = null
      onPick = null
    },
  }
}

export const MentionSuggestion = Extension.create({
  name: 'superopsMentionSuggestion',

  addProseMirrorPlugins() {
    return [
      Suggestion({
        editor: this.editor,
        char: '@',
        // Only at a word boundary, or an email address typed into a document
        // would open a picker on every `@`.
        allowSpaces: false,
        startOfLine: false,

        items: async ({ query }: { query: string }) => {
          try {
            const res = await userApi.search(query)
            return (res.data ?? []).slice(0, 8).map((u) => ({
              id: u.id,
              label: u.full_name || u.username,
            }))
          } catch {
            // A failed lookup shows an empty picker rather than an error: the
            // author is mid-sentence, and interrupting them to say the
            // directory is unreachable helps nobody.
            return []
          }
        },

        command: ({ editor, range, props }: { editor: any; range: any; props: Candidate }) => {
          editor
            .chain()
            .focus()
            .insertContentAt(range, [
              // THE NAME IS NOT STORED. Only the id.
              { type: 'mention', attrs: { refId: props.id } },
              { type: 'text', text: ' ' },
            ])
            .run()
        },

        render: renderPicker,
      }),
    ]
  },
})
