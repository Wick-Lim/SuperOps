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
import { ProjectionScheduler } from '../lib/collab/projectionScheduler'

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

// THE PROPS COME FROM THE DECLARATION, they are not restated here.
//
// This file, its platform twin and Editor.d.ts each declared EditorProps
// independently. `tsc` checks CALLERS against the .d.ts and each implementation
// against its own copy, and nothing cross-checked the three — so adding a
// required prop here type-checked, passed every test, and crashed on mount,
// because no caller could ever pass it. Importing the declaration makes the
// .d.ts the single contract and turns that into a compile error.
//
// A type-only import is erased before Metro sees it, so this cannot re-create
// the resolution bug the .d.ts exists to avoid.
import type { EditorProps, EditorProps as DeclaredEditorProps } from './Editor'

export type { EditorProps }

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
  doc, awareness, editable, projectable, user, onProject, registerProjectionFlush,
  seq, fileId, catchUp,
}: EditorProps) {
  // One resolver per open document. Thrown away with the editor, which is what
  // keeps its cache scoped to the file it was authorized against — a global one
  // would let a name learned through a document you may read leak into one you
  // may not.
  const resolver = useMemo(() => new RefResolver(fileId ?? ''), [fileId])
  const seqRef = useRef(seq)
  seqRef.current = seq
  const projectableRef = useRef(projectable)
  projectableRef.current = projectable
  // The caret extension reads this only when a document/awareness boundary
  // creates a new editor. Profile changes are applied imperatively below so an
  // ordinary parent render cannot replace the editor and discard its dirty
  // projection scheduler while an edit is awaiting acknowledgement.
  const userRef = useRef(user)
  userRef.current = user

  const extensions = useMemo(
    () => [
      StarterKit.configure({
        // See the header: Yjs owns undo.
        undoRedo: false,
      }),
      Collaboration.configure({ document: doc }),
      CollaborationCaret.configure({ provider: { awareness }, user: userRef.current }),
      // References. The nodes carry {refType, refId} and nothing else; the
      // label is painted per caller as a decoration, so it never enters the
      // document, the CRDT, the projection, or a copy-paste.
      Mention,
      DriveEmbed,
      IssueEmbed,
      RefLabels(resolver),
      MentionSuggestion,
    ],
    [doc, awareness, resolver],
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
    [extensions],
  )

  useEffect(() => {
    editor?.commands.updateUser(user)
  }, [editor, user.id, user.name, user.color])

  const schedulerRef = useRef<ProjectionScheduler<Projection> | null>(null)
  useEffect(() => {
    if (!editor) return
    const scheduler = new ProjectionScheduler<Projection>({
      delayMs: PROJECT_DEBOUNCE_MS,
      readState: () => ({
        seq: seqRef.current,
        synced: projectableRef.current,
        pending: !projectableRef.current,
      }),
      build: (projectionSeq) => extract(editor.getJSON(), projectionSeq),
      publish: onProject,
    })
    schedulerRef.current = scheduler
    const unregisterFlush = registerProjectionFlush(() => scheduler.flush())
    return () => {
      unregisterFlush()
      // Flush only when the provider has acknowledged a newer sequence. An
      // unsafe close discards derived work, never labels pending content N.
      scheduler.flush()
      scheduler.dispose()
      if (schedulerRef.current === scheduler) schedulerRef.current = null
    }
  }, [editor, onProject, registerProjectionFlush])

  useEffect(() => {
    schedulerRef.current?.notify()
  }, [seq, projectable])

  // Publish on settle. The projection is derived state — losing one costs
  // search until the next edit and costs zero content — so a failure here is
  // deliberately not surfaced to the writer.
  useEffect(() => {
    if (!editor || !editable) return
    const schedule = () => schedulerRef.current?.request()
    const flushProjection = () => schedulerRef.current?.flush()
    editor.on('update', schedule)
    // On blur as well as on idle: somebody who types a sentence and switches
    // tab has stopped editing, and waiting for the debounce would miss it.
    editor.on('blur', flushProjection)
    return () => {
      editor.off('update', schedule)
      editor.off('blur', flushProjection)
    }
  }, [editor, editable])

  // CATCH UP once per request, when the stored projection is behind and this
  // caller may write.
  //
  // THIS IS THE ONLY REPAIR PATH FOR A BLOCK DOCUMENT that does not require
  // somebody to close the tab. The other two — `update` and `blur` — fire on
  // interaction, and the unmount flush needs a writer to open AND close the
  // file. Neither answers the server's `collab.project` request, and neither
  // fires at mount.
  //
  // Delayed rather than immediate: the Y.Doc is empty until the provider has
  // fetched state, and projecting then would write an EMPTY body over a good
  // one — the server's monotonic guard cannot catch it, because the seq is the
  // log head either way. Waiting for a non-empty document is the check that
  // actually distinguishes "loaded and empty" from "not loaded yet".
  const caughtUp = useRef(0)
  useEffect(() => {
    if (!editor || !editable || !projectable || !catchUp || caughtUp.current === catchUp) return
    const timer = setTimeout(() => {
      if (caughtUp.current === catchUp) return
      const projection = extract(editor.getJSON(), seqRef.current)
      // Still empty — see worthPublishing. Either the document genuinely is, in
      // which case there is nothing to catch up, or state has not arrived, in
      // which case publishing would destroy the stored text.
      if (!worthPublishing(projection)) return
      if (onProject(projection)) caughtUp.current = catchUp
    }, 2500)
    return () => clearTimeout(timer)
  }, [editor, editable, projectable, catchUp, onProject, seq])

  useEffect(() => {
    editor?.setEditable(editable)
  }, [editor, editable])

  return <EditorContent editor={editor} className="superops-editor" />
}

// THE CONTRACT, ASSERTED. Importing the props type is not enough on its own:
// an implementation can widen its OWN parameter type and callers still type-
// check against the declaration, which is how a required `tenantId` nobody
// could pass compiled clean and crashed on mount.
//
// This line fails to compile whenever this file's component cannot be called
// with exactly the props the .d.ts promises callers they may pass.
const _satisfiesDeclaration: (props: DeclaredEditorProps) => unknown = Editor
void _satisfiesDeclaration
