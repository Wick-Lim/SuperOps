import type { Projection } from './projection'

/**
 * Whether a catch-up projection is safe to publish.
 *
 * THE ONE RULE, in one place. It was written twice — once in the block editor
 * and once in the sheet/canvas screen — with two different predicates for the
 * same idea, which is the shape of every silent drift in this codebase: both
 * look right, and the day they disagree nothing reports it.
 *
 * A catch-up fires when the stored projection is behind the log, and the danger
 * is publishing an EMPTY body over a good one. That happens when the CRDT has
 * not received its state yet — and the server cannot defend against it, because
 * the seq is the log head either way, so the monotonic guard accepts the write.
 *
 * An empty result is therefore refused rather than published. Either the object
 * genuinely is empty, in which case there is nothing to repair, or it has not
 * loaded, in which case publishing would destroy the text. Both readings say
 * the same thing: do nothing.
 *
 * This applies ONLY to catch-up. An edit that deletes the last word of a
 * document must still project an empty body — that is a real change, and the
 * ordinary debounce publishes it.
 */
export function worthPublishing(projection: Projection): boolean {
  return (
    projection.body_text.trim() !== '' ||
    projection.outline.length > 0 ||
    projection.refs.length > 0
  )
}
