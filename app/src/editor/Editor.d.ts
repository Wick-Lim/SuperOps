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
  /** The server decides; a read-only surface is rendered, not failed on submit. */
  editable: boolean
  user: { name: string; color: string }
  onProject: (projection: Projection) => void
  seq: number
}

declare const Editor: (props: EditorProps) => JSX.Element
export default Editor
