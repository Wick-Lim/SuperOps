export interface ProjectionReadiness {
  /** Highest contiguous collaboration sequence applied by the provider. */
  seq: number
  synced: boolean
  pending: boolean
}

interface ProjectionSchedulerOptions<T> {
  delayMs: number
  readState: () => ProjectionReadiness
  /** Rebuild at publish time so delayed work contains the latest CRDT state. */
  build: (seq: number) => T
  /** False keeps the dirty intent for the next readiness notification. */
  publish: (projection: T) => boolean
}

/**
 * Debounces derived projections without ever assigning an unacknowledged edit
 * the provider's old sequence. A local edit at N cannot be published before a
 * fully synchronized contiguous watermark of at least N+1 exists.
 */
export class ProjectionScheduler<T> {
  private readonly delayMs: number
  private readonly readState: () => ProjectionReadiness
  private readonly build: (seq: number) => T
  private readonly publish: (projection: T) => boolean
  private timer: ReturnType<typeof setTimeout> | null = null
  private dirty = false
  private minimumSeq = 0
  private disposed = false

  constructor(options: ProjectionSchedulerOptions<T>) {
    this.delayMs = options.delayMs
    this.readState = options.readState
    this.build = options.build
    this.publish = options.publish
  }

  request(): void {
    if (this.disposed) return
    const state = this.readState()
    this.dirty = true
    this.minimumSeq = Math.max(this.minimumSeq, state.seq + 1)
    this.clearTimer()
    this.timer = setTimeout(() => {
      this.timer = null
      this.tryPublish()
    }, this.delayMs)
  }

  /** Re-evaluates retained work after status or contiguous seq changes. */
  notify(): boolean {
    if (this.disposed) return false
    // An acknowledgement may arrive well before the editor has settled. Keep
    // the original debounce; notify is immediate only for a timer that already
    // fired while publication was unsafe.
    if (this.timer) return false
    return this.tryPublish()
  }

  /** Safe unmount/blur flush: unsafe work remains dirty and is never emitted. */
  flush(): boolean {
    if (this.disposed) return false
    this.clearTimer()
    return this.tryPublish()
  }

  dispose(): void {
    if (this.disposed) return
    this.disposed = true
    this.clearTimer()
    this.dirty = false
  }

  private tryPublish(): boolean {
    if (!this.dirty) return false
    const state = this.readState()
    if (!state.synced || state.pending || state.seq < this.minimumSeq) return false
    const projection = this.build(state.seq)
    if (!this.publish(projection)) return false
    this.dirty = false
    this.minimumSeq = 0
    return true
  }

  private clearTimer(): void {
    if (!this.timer) return
    clearTimeout(this.timer)
    this.timer = null
  }
}
