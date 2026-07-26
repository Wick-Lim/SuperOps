import { driveApi } from '../api/drive'
import type { ResolvedRef } from '../api/drive'

/**
 * Resolving reference labels, per caller.
 *
 * A reference node carries {refType, refId} and nothing else, so the label has
 * to come from somewhere at render time. This is that somewhere, and the two
 * things it does that a naive fetch would not are both about correctness rather
 * than speed.
 *
 * IT BATCHES. A document with forty mentions would otherwise make forty
 * requests, each doing its own capability check, on every open.
 *
 * IT CACHES PER FILE, NOT GLOBALLY. The answer to "may I see this file's name"
 * depends on the caller, and the caller is fixed for a session — but it also
 * depends on which document is asking, because the resolve endpoint authorizes
 * against the document as well as the target. A global cache would let a name
 * learned through a document you may read leak into one you may not.
 */

/** What a resolved reference looks like to a NodeView. */
export interface RefLabel {
  /** Absent when access was denied — NOT an empty string. The difference is
   * "you may not see this" versus "this has no name", and a placeholder should
   * say the first. */
  title?: string
  denied: boolean
}

type Pending = {
  resolve: (label: RefLabel) => void
  key: string
  refType: string
  refId: string
}

const DENIED: RefLabel = { denied: true }

/**
 * One resolver per open document.
 *
 * Created by the editor and thrown away with it, which is what keeps the cache
 * scoped to the file it was authorized against.
 */
export class RefResolver {
  private cache = new Map<string, RefLabel>()
  private queue: Pending[] = []
  private timer: ReturnType<typeof setTimeout> | null = null

  constructor(private readonly fileId: string) {}

  /** Resolves one reference, batching with anything asked for in the same tick. */
  resolve(refType: string, refId: string): Promise<RefLabel> {
    const key = `${refType}:${refId}`
    const cached = this.cache.get(key)
    if (cached) return Promise.resolve(cached)

    return new Promise<RefLabel>((resolve) => {
      this.queue.push({ resolve, key, refType, refId })
      if (this.timer) return
      // One frame. Long enough to collect every reference a document renders on
      // open, short enough that a placeholder does not visibly linger.
      this.timer = setTimeout(() => void this.flush(), 16)
    })
  }

  /** Drops the cache, for when a share changed under the reader. */
  invalidate() {
    this.cache.clear()
  }

  private async flush() {
    this.timer = null
    const batch = this.queue.splice(0, this.queue.length)
    if (batch.length === 0) return

    // Deduplicated: the same person mentioned six times is one lookup.
    const unique = new Map<string, { ref_type: string; ref_id: string }>()
    for (const p of batch) {
      unique.set(p.key, { ref_type: p.refType, ref_id: p.refId })
    }

    let answers: ResolvedRef[] = []
    try {
      const res = await driveApi.resolveRefs(this.fileId, [...unique.values()])
      answers = res.data ?? []
    } catch {
      // A failed resolve is DENIED, not "unknown". Failing open here would show
      // a stale or guessed label for a target the reader may not be allowed to
      // see, which is exactly what the placeholder design prevents.
      for (const p of batch) p.resolve(DENIED)
      return
    }

    const byKey = new Map<string, RefLabel>()
    for (const a of answers) {
      byKey.set(`${a.ref_type}:${a.ref_id}`, {
        // `title` is ABSENT on a denial — the server omits the field rather
        // than sending "" — so this is a presence check, not a truthiness one.
        title: a.access === 'granted' ? a.title : undefined,
        denied: a.access !== 'granted',
      })
    }

    for (const p of batch) {
      const label = byKey.get(p.key) ?? DENIED
      this.cache.set(p.key, label)
      p.resolve(label)
    }
  }
}

/** What a placeholder says when the reader may not see the target.
 *
 * Deliberately not "unknown" or an empty space: the reader should know there IS
 * something there and that they cannot see it, because a document full of
 * invisible gaps reads as broken rather than as restricted. */
export function deniedLabel(refType: string): string {
  switch (refType) {
    case 'user':
      return '@someone'
    case 'issue':
      return 'an issue'
    default:
      return 'a file you cannot open'
  }
}
