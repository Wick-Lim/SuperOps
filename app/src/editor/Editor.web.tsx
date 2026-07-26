import React, { useEffect, useMemo, useRef } from 'react'
import { useEditor, EditorContent } from '@tiptap/react'
import StarterKit from '@tiptap/starter-kit'
import Collaboration from '@tiptap/extension-collaboration'
import CollaborationCaret from '@tiptap/extension-collaboration-caret'
import type * as Y from 'yjs'
import type { Awareness } from 'y-protocols/awareness'
import { worthPublishing } from './catchup'
import { extract, type Projection } from './projection'
import { Mention, DriveEmbed, IssueEmbed } from './extensions/refs'
import { RefLabels } from './extensions/refViews'
import { RefResolver } from './refResolver'
import { MentionSuggestion } from './extensions/mentionSuggestion'

/**
 * The block editor, web only.
 *
 * TipTap/ProseMirror is DOM-only, so this file exists only in the web bundle —
 * Metro's platform resolution keeps roughly 400 KB of editor out of the iOS and
 * Android bundles, and the native sibling renders the projection read-only.
 * That is ROADMAP §3 option (1), taken as recommended.
 *
 * Two things here are load-bearing rather than configuration:
 *
 *   - HISTORY IS OFF in StarterKit and undo comes from Yjs. Naive undo in a
 *     collaborative editor undoes OTHER PEOPLE'S edits: ProseMirror's history
 *     plugin knows only the local step order, so ctrl-Z after a colleague typed
 *     reverts their paragraph. The Collaboration extension installs a Yjs
 *     UndoManager scoped to this client's own changes, and wiring it is the
 *     only thing that makes undo mean what a user thinks it means.
 *   - The document is a Y.XmlFragment owned by the provider, so this component
 *     never loads or saves. There is no "save" button and there is nothing to
 *     press it for.
 */

export interface EditorProps {
  doc: Y.Doc
  awareness: Awareness
  /** The Drive object, for resolving reference labels against THIS document —
   * the resolve endpoint authorizes on the document as well as the target. */
  fileId?: string
  /** Publish a projection once the document has loaded, because the stored one
   * is behind the log. Raised by the descriptor's head_seq/projection_seq gap
   * on open and by the server's `collab.project` request — the two backstops
   * for a document whose browser was killed before its debounce fired, since
   * the server cannot produce content on its own.
   *
   * A COUNTER rather than a flag: the server re-asks, and a consumed boolean
   * would swallow every request after the first. 0 means nothing is asked. */
  catchUp?: number
  editable: boolean
  user: { name: string; color: string }
  /** Called when the document settles, with the projection to publish. */
  onProject: (projection: Projection) => void
  /** The log position the projection describes. */
  seq: number
}

/**
 * How long the document must be still before its projection is published.
 *
 * Short enough that closing a tab after a sentence leaves the document
 * searchable; long enough that typing a paragraph is one projection rather than
 * forty. The server's monotonic write makes a burst harmless anyway — the
 * losers match zero rows — so this is about request volume, not correctness.
 */
const PROJECT_DEBOUNCE_MS = 2000

export default function Editor({
  doc, awareness, editable, user, onProject, seq, fileId, catchUp,
}: EditorProps) {
  // One resolver per open document. Thrown away with the editor, which is what
  // keeps its cache scoped to the file it was authorized against — a global one
  // would let a name learned through a document you may read leak into one you
  // may not.
  const resolver = useMemo(() => new RefResolver(fileId ?? ''), [fileId])
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)
  const seqRef = useRef(seq)
  seqRef.current = seq

  const extensions = useMemo(
    () => [
      StarterKit.configure({
        // See the header: Yjs owns undo.
        undoRedo: false,
      }),
      Collaboration.configure({ document: doc }),
      CollaborationCaret.configure({ provider: { awareness }, user }),
      // References. The nodes carry {refType, refId} and nothing else; the
      // label is painted per caller as a decoration, so it never enters the
      // document, the CRDT, the projection, or a copy-paste.
      Mention,
      DriveEmbed,
      IssueEmbed,
      RefLabels(resolver),
      MentionSuggestion,
    ],
    [doc, awareness, user, resolver],
  )

  const editor = useEditor(
    {
      extensions,
      editable,
      // The server decides what a caller may do and the descriptor carries it,
      // so a read-only surface is RENDERED read-only rather than failing on
      // submit. immediatelyRender is off because this component also runs under
      // SSR-less hydration in the web build.
      immediatelyRender: false,
    },
    [extensions, editable],
  )

  // Publish on settle. The projection is derived state — losing one costs
  // search until the next edit and costs zero content — so a failure here is
  // deliberately not surfaced to the writer.
  useEffect(() => {
    if (!editor || !editable) return
    const schedule = () => {
      if (timer.current) clearTimeout(timer.current)
      timer.current = setTimeout(() => {
        onProject(extract(editor.getJSON(), seqRef.current))
      }, PROJECT_DEBOUNCE_MS)
    }
    editor.on('update', schedule)
    // On blur as well as on idle: somebody who types a sentence and switches
    // tab has stopped editing, and waiting for the debounce would miss it.
    editor.on('blur', schedule)
    return () => {
      editor.off('update', schedule)
      editor.off('blur', schedule)
      if (timer.current) clearTimeout(timer.current)
    }
  }, [editor, editable, onProject])

  // CATCH UP once, when the stored projection is behind and this caller may
  // write.
  //
  // Delayed rather than immediate: the Y.Doc is empty until the provider has
  // fetched state, and projecting then would write an EMPTY body over a good
  // one — the server's monotonic guard cannot catch it, because the seq is the
  // log head either way. Waiting for a non-empty document is the check that
  // actually distinguishes "loaded and empty" from "not loaded yet".
  const caughtUp = useRef(0)
  useEffect(() => {
    if (!editor || !editable || !catchUp || caughtUp.current === catchUp) return
    const timer = setTimeout(() => {
      if (caughtUp.current === catchUp) return
      const projection = extract(editor.getJSON(), seqRef.current)
      // Still empty — see worthPublishing. Either the document genuinely is, in
      // which case there is nothing to catch up, or state has not arrived, in
      // which case publishing would destroy the stored text.
      if (!worthPublishing(projection)) return
      caughtUp.current = catchUp
      onProject(projection)
    }, 2500)
    return () => clearTimeout(timer)
  }, [editor, editable, catchUp, onProject])

  // And once more on unmount, which is the case the reconciler exists for: a
  // document edited and closed before the debounce fired would otherwise stay
  // unsearchable until somebody opened it again.
  useEffect(() => {
    return () => {
      if (editor && editable) {
        try {
          onProject(extract(editor.getJSON(), seqRef.current))
        } catch {
          /* the editor may already be torn down; the reconciler covers it */
        }
      }
    }
  }, [editor, editable, onProject])

  useEffect(() => {
    editor?.setEditable(editable)
  }, [editor, editable])

  return <EditorContent editor={editor} className="superops-editor" />
}
