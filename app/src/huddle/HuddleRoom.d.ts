/**
 * The platform split for the call surface, visible to TypeScript and INVISIBLE
 * to Metro.
 *
 * A `.d.ts` for the same reason the editor's is: an `Editor.ts` re-export was
 * resolved by Metro AHEAD of the platform variants once already, and the web
 * build silently shipped the read-only renderer. `.d.ts` is not in Metro's
 * sourceExts, so the variants are the only candidates.
 */
import type { HuddleSession } from '../api/huddles'

export interface HuddleRoomProps {
  session: HuddleSession
  onLeave: () => void
}

declare const HuddleRoom: (props: HuddleRoomProps) => JSX.Element
export default HuddleRoom
