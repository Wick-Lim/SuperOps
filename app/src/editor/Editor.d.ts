/**
 * The platform split, made visible to TypeScript and INVISIBLE to Metro.
 *
 * Metro resolves `./Editor` to `Editor.web.tsx` on web and `Editor.native.tsx`
 * on iOS and Android. `tsc` does not do platform resolution, so without a
 * declaration here the import is an unresolved module and every call site goes
 * untyped.
 *
 * IT IS A `.d.ts` ON PURPOSE, and this is not a style choice. The obvious
 * version — an `Editor.ts` re-exporting one implementation — WAS WRITTEN FIRST
 * AND WAS WRONG: Metro resolved `./Editor` to that file ahead of the platform
 * variants, so the web build shipped the read-only native renderer and the
 * editor silently did not exist. The bundle check caught it; nothing else
 * would have, because both files type-check and both render something.
 *
 * `.d.ts` is not in Metro's sourceExts, so Metro cannot pick it and the
 * platform variants are the only candidates.
 */
import type * as Y from 'yjs'
import type { Awareness } from 'y-protocols/awareness'
import type { Projection } from './projection'

export interface EditorProps {
  doc: Y.Doc
  awareness: Awareness
  /** The Drive object, for resolving reference labels against THIS document.
   * Unused on native — the read-only renderer shows a reference as its
   * placeholder rather than resolving it, because resolving costs a request per
   * open on a connection that may be a phone's. */
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
  /** The server decides; a read-only surface is rendered, not failed on submit. */
  editable: boolean
  /** True only at a fully synchronized provider watermark with no local bytes pending. */
  projectable: boolean
  user: { id: string; name: string; color: string }
  /** Returns false when the provider changed state before the projection reached the screen. */
  onProject: (projection: Projection) => boolean
  /** Lets the provider owner flush derived work before tearing transport down. */
  registerProjectionFlush: (flush: () => boolean) => () => void
  seq: number
}

declare const Editor: (props: EditorProps) => JSX.Element
export default Editor
