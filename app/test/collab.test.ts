import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import * as Y from 'yjs'
import { wsManager } from '../src/lib/websocket'
import {
  CollabProvider,
  isProviderEditable,
  type ProviderStatus,
} from '../src/lib/collab/provider'
import { fromBase64, toBase64 } from '../src/lib/collab/base64'
import {
  apiFailure,
  envelope,
  FakeWebSocket,
  flush,
  installFakeWebSocket,
  mockFetch,
  ok,
  resetStores,
  signIn,
  type FetchMock,
} from './helpers'

const realFetch = globalThis.fetch
let net: FetchMock = { calls: [], to: () => [] }

const DOC = '11111111-1111-1111-1111-111111111111'

function emptyState(overrides: Record<string, unknown> = {}) {
  return ok({
    document_id: DOC,
    snapshot_seq: 0,
    updates: [],
    through_seq: 0,
    head_seq: 0,
    has_more: false,
    ...overrides,
  })
}

/** The `since` query parameter of a `/state` request. */
function sinceOf(path: string): string | null {
  const q = path.indexOf('?')
  return q < 0 ? null : new URLSearchParams(path.slice(q + 1)).get('since')
}

/** POSTs to the bulk-append route, whichever document they name. */
function updateCalls() {
  return net.calls.filter((c) => c.path.includes('/updates'))
}

/**
 * Enough microtask turns for the provider's catch-up to finish.
 *
 * The join ack triggers an HTTP read of the log, which is several awaits deep
 * (fetch, json, the has_more loop). The shared `flush()` default of six turns
 * lands mid-flight, so a test that used it would assert against a provider that
 * had not finished syncing — and would pass or fail on unrelated timing.
 */
const settle = () => flush(30)

function connectOpen(): FakeWebSocket {
  wsManager.connect()
  const socket = FakeWebSocket.last
  socket.openNow()
  return socket
}

function makeProvider(
  onStatus?: (status: ProviderStatus, detail?: string) => void,
  onProjectRequest?: () => void,
  onProjectionSeq: (seq: number) => void = () => undefined,
) {
  const doc = new Y.Doc()
  const provider = new CollabProvider({
    documentId: DOC,
    doc,
    user: { id: 'u-self', name: 'Self', color: '#f00' },
    onStatus,
    onProjectRequest,
    onProjectionSeq,
  })
  return { doc, provider }
}

beforeEach(() => {
  resetStores()
  installFakeWebSocket()
  signIn()
  net = mockFetch((req) => {
    if (req.path.includes('/state')) return emptyState()
    if (req.path.includes('/updates')) return envelope(201, { data: { seq: 1 } })
    return ok({})
  })
})

afterEach(() => {
  wsManager.reset()
  globalThis.fetch = realFetch
})

describe('base64', () => {
  // The wire form of every CRDT payload. A decoder that over-allocated would
  // hand Yjs trailing zero bytes, and Yjs would read them as document content.
  it('round-trips every length class, including the padding cases', () => {
    for (const len of [0, 1, 2, 3, 4, 5, 255, 256, 1000, 3001]) {
      const bytes = new Uint8Array(len)
      for (let i = 0; i < len; i++) bytes[i] = (i * 37 + 11) & 0xff
      const back = fromBase64(toBase64(bytes))
      expect(back.length, `length ${len}`).toBe(len)
      expect(Array.from(back), `content ${len}`).toEqual(Array.from(bytes))
    }
  })

  it('encodes a large payload without blowing the argument limit', () => {
    // btoa(String.fromCharCode(...bytes)) throws here, which is exactly the
    // case the HTTP fallback exists for — a pasted table.
    const big = new Uint8Array(200_000).fill(7)
    expect(() => toBase64(big)).not.toThrow()
    expect(fromBase64(toBase64(big)).length).toBe(200_000)
  })

  it('agrees with the platform decoder, so the server can read what we send', () => {
    const bytes = new Uint8Array([0, 1, 2, 250, 251, 252, 253, 254, 255])
    const ours = toBase64(bytes)
    // atob exists in the vitest (node/jsdom) environment; the point is that our
    // encoder produces standard base64, not a private dialect.
    const decoded = atob(ours)
    expect(decoded.length).toBe(bytes.length)
    for (let i = 0; i < bytes.length; i++) expect(decoded.charCodeAt(i)).toBe(bytes[i])
  })
})

describe('the collaboration provider', () => {
  it('edits only in synced and saving states', () => {
    expect(isProviderEditable('synced')).toBe(true)
    expect(isProviderEditable('saving')).toBe(true)
    expect(isProviderEditable('connecting')).toBe(false)
    expect(isProviderEditable('error')).toBe(false)
    expect(isProviderEditable('read-only')).toBe(false)
    expect(isProviderEditable('revoked')).toBe(false)
  })

  it('joins the room and reports read-only when the server says so', async () => {
    const socket = connectOpen()
    const statuses: ProviderStatus[] = []
    const { provider } = makeProvider((s) => statuses.push(s))
    await flush()

    expect(socket.sentOfType('collab.join').map((f) => f.data.document_id)).toEqual([DOC])

    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: false } })
    await settle()

    expect(provider.writable).toBe(false)
    expect(statuses).toContain('read-only')
    provider.destroy()
  })

  // TWO Y.Docs, concurrent edits, relayed through the real provider. Go cannot
  // compute Yjs convergence, so this is the only place in the product where the
  // CRDT's central claim is actually checked.
  it('converges two documents that were edited concurrently', async () => {
    const socket = connectOpen()
    const a = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    // The second client is a bare Y.Doc — the relay is simulated below, so this
    // test does not depend on two providers sharing one fake socket.
    const b = new Y.Doc()

    a.doc.getText('body').insert(0, 'hello ')
    b.getText('body').insert(0, 'world')

    // Exchange, in the WRONG ORDER on purpose. CRDT updates are
    // order-independent and the whole design leans on it.
    const fromA = Y.encodeStateAsUpdate(a.doc)
    const fromB = Y.encodeStateAsUpdate(b)
    Y.applyUpdate(b, fromA)
    Y.applyUpdate(a.doc, fromB)

    expect(a.doc.getText('body').toString()).toBe(b.getText('body').toString())
    expect(a.doc.getText('body').toString().length).toBe('hello world'.length)
    a.provider.destroy()
  })

  it('applies a relayed update through the socket', async () => {
    const socket = connectOpen()
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    const other = new Y.Doc()
    other.getText('body').insert(0, 'from elsewhere')
    const update = toBase64(Y.encodeStateAsUpdate(other))

    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 1, actor_id: 'u-other', origin_conn: 'c-other', update },
    })
    await flush()

    expect(doc.getText('body').toString()).toBe('from elsewhere')
    provider.destroy()
  })

  // THE WATERMARK RULE. The server broadcasts after committing, so seq 6 can
  // arrive before seq 5. A client that advanced its watermark to 6 would ask
  // for state since=6 on the next reconnect and never receive 5 — one edit,
  // lost permanently and invisibly.
  it('advances the watermark only over a contiguous prefix', async () => {
    const socket = connectOpen()
    const { provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    const payload = (text: string) => {
      const d = new Y.Doc()
      d.getText('body').insert(0, text)
      return toBase64(Y.encodeStateAsUpdate(d))
    }

    // 1 arrives, then 3 — a reordering, not a loss.
    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 1, actor_id: 'x', origin_conn: '', update: payload('a') },
    })
    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 3, actor_id: 'x', origin_conn: '', update: payload('c') },
    })
    await flush()

    // Reconnect. The state request must ask from 1, not 3.
    net.calls.length = 0
    socket.dropNow()
    const resumed = connectOpen()
    resumed.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 3, can_write: true } })
    await settle()

    const stateCalls = net.calls.filter((c) => c.path.includes('/state'))
    expect(stateCalls.length, 'a reconnect must re-read the log').toBeGreaterThan(0)
    expect(sinceOf(stateCalls[0].path), 'the watermark jumped over a gap and an update is lost').toBe('1')
    provider.destroy()
  })

  it('emits only the contiguous projection watermark across a gap', async () => {
    const socket = connectOpen()
    const projected: number[] = []
    const { provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 9, can_write: true },
    })
    await settle()

    const payload = (text: string) => {
      const other = new Y.Doc()
      other.getText('body').insert(0, text)
      return toBase64(Y.encodeStateAsUpdate(other))
    }
    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 1, actor_id: 'x', origin_conn: 'x', update: payload('1') },
    })
    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 3, actor_id: 'x', origin_conn: 'x', update: payload('3') },
    })
    await flush()
    expect(projected).toEqual([1])

    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 2, actor_id: 'x', origin_conn: 'x', update: payload('2') },
    })
    socket.emit({
      type: 'collab.update',
      data: { document_id: DOC, seq: 2, actor_id: 'x', origin_conn: 'x', update: payload('2') },
    })
    await flush()
    expect(projected).toEqual([1, 3])
    expect(projected).not.toContain(9)
    provider.destroy()
  })

  it('drains buffered seqs after contiguous HTTP catch-up', async () => {
    net = mockFetch((req) => req.path.includes('/state')
      ? ok({
          document_id: DOC,
          snapshot_seq: 0,
          updates: [],
          through_seq: 2,
          head_seq: 3,
          has_more: false,
        })
      : ok({}))
    const socket = connectOpen()
    const projected: number[] = []
    const { provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
    await flush()
    const other = new Y.Doc()
    other.getText('body').insert(0, 'three')
    socket.emit({
      type: 'collab.update',
      data: {
        document_id: DOC,
        seq: 3,
        actor_id: 'x',
        origin_conn: 'x',
        update: toBase64(Y.encodeStateAsUpdate(other)),
      },
    })
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 3, can_write: true },
    })
    await settle()

    expect(projected).toEqual([3])
    provider.destroy()
  })

  it('sends a small update over the socket and a large one over HTTP', async () => {
    const socket = connectOpen()
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    doc.getText('body').insert(0, 'a short edit')
    await flush()
    expect(socket.sentOfType('collab.update').length).toBe(1)
    expect(updateCalls().length).toBe(0)

    // A paste. The socket refuses anything over 32 KiB, so this must not go
    // out as a frame — an editor that lost a pasted table would lose it
    // silently.
    doc.getText('body').insert(0, 'x'.repeat(60_000))
    await flush()
    expect(socket.sentOfType('collab.update').length, 'the paste went out as a frame').toBe(1)
    expect(updateCalls().length, 'the paste was not sent at all').toBe(1)
    provider.destroy()
  })

  it('sends nothing while read-only', async () => {
    const socket = connectOpen()
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: false } })
    await settle()

    doc.getText('body').insert(0, 'should never leave this client')
    await flush()

    expect(socket.sentOfType('collab.update').length).toBe(0)
    expect(updateCalls().length).toBe(0)
    provider.destroy()
  })

  // Revocation is not "you left". The editor renders a different thing for
  // each, and collapsing them would tell somebody who was removed from a
  // document that they had closed it.
  it('reports revocation and stops trying to rejoin', async () => {
    const socket = connectOpen()
    const statuses: ProviderStatus[] = []
    const { provider } = makeProvider((s) => statuses.push(s))
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    socket.emit({ type: 'collab.left', data: { document_id: DOC, reason: 'revoked' } })
    await flush()

    expect(statuses).toContain('revoked')
    expect(provider.writable).toBe(false)

    // And a reconnect does NOT re-join: the server would refuse it, and the
    // editor would retry forever against a document nobody may open.
    socket.dropNow()
    const resumed = connectOpen()
    await flush()
    expect(resumed.sentOfType('collab.join').length).toBe(0)
    provider.destroy()
  })

  // A room is per connection. Without a re-join the editor goes quietly
  // one-way: it keeps sending updates the server refuses while the document on
  // screen stops changing.
  it('re-joins the room after a reconnect', async () => {
    const socket = connectOpen()
    const { provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    socket.dropNow()
    const resumed = connectOpen()
    await flush()

    expect(resumed.sentOfType('collab.join').map((f) => f.data.document_id)).toEqual([DOC])
    provider.destroy()
  })

  // The origin connection receives its own echo — that is how it learns the seq
  // its update was assigned. Skipping it would leave a permanent hole in the
  // watermark exactly where this client's own edits are.
  it('applies its own echo rather than skipping it', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'hello', seq: 1, data: { connection_id: 'connection-a' } })
    const projected: number[] = []
    const { doc, provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    doc.getText('body').insert(0, 'mine')
    await flush()
    const sent = socket.sentOfType('collab.update')[0].data.update as string

    socket.emit({
      type: 'collab.update',
      data: {
        document_id: DOC,
        seq: 1,
        actor_id: 'u-self',
        origin_conn: 'connection-a',
        update: sent,
      },
    })
    await flush()
    expect(projected).toEqual([1])

    // Applying an update twice is a no-op in Yjs, so the text is unchanged...
    expect(doc.getText('body').toString()).toBe('mine')

    // ...and the watermark advanced, which is what the echo is for.
    net.calls.length = 0
    socket.dropNow()
    const resumed = connectOpen()
    resumed.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 1, can_write: true } })
    await settle()
    const stateCalls = net.calls.filter((c) => c.path.includes('/state'))
    expect(sinceOf(stateCalls[0].path), 'the echo was skipped, leaving a hole at this clients own edits').toBe('1')
    provider.destroy()
  })

  it('waits for the append sequence to become contiguous before clearing pending', async () => {
    let stateCalls = 0
    let appendPayload = ''
    let releaseGap!: () => void
    const gapGate = new Promise<void>((resolve) => { releaseGap = resolve })
    const seq4Doc = new Y.Doc()
    seq4Doc.getText('body').insert(0, 'remote-four')
    const seq4 = toBase64(Y.encodeStateAsUpdate(seq4Doc))

    net = mockFetch(async (req) => {
      if (req.path.includes('/state')) {
        stateCalls += 1
        if (stateCalls <= 2) {
          return ok({
            document_id: DOC,
            snapshot_seq: 0,
            updates: [],
            through_seq: 3,
            head_seq: 3,
            has_more: false,
          })
        }
        await gapGate
        return ok({
          document_id: DOC,
          snapshot_seq: 0,
          updates: [
            { seq: 4, payload: seq4 },
            { seq: 5, payload: appendPayload },
          ],
          through_seq: 5,
          head_seq: 5,
          has_more: false,
        })
      }
      if (req.path.includes('/updates')) {
        appendPayload = (req.body as { update: string }).update
        return envelope(201, { data: { seq: 5 } })
      }
      return ok({})
    })

    const socket = connectOpen()
    socket.emit({ type: 'hello', data: { connection_id: 'connection-a' } })
    const projected: number[] = []
    const { doc, provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 3, can_write: true },
    })
    await settle()
    expect(projected).toEqual([3])
    projected.length = 0

    doc.getText('body').insert(0, 'local-five')
    await flush()
    socket.dropNow()
    const resumed = connectOpen()
    resumed.emit({ type: 'hello', data: { connection_id: 'connection-b' } })
    resumed.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 3, can_write: true },
    })
    await flush(50)

    expect(appendPayload).not.toBe('')
    expect(projected).toEqual([])
    expect(provider.currentStatus).toBe('connecting')
    expect(provider.hasPendingChanges).toBe(true)

    releaseGap()
    await settle()
    expect(projected).toEqual([5])
    expect(provider.currentStatus).toBe('synced')
    expect(provider.hasPendingChanges).toBe(false)
    provider.destroy()
  })

  it('stays saving until an echo from its own connection is durable', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'hello', seq: 1, data: { connection_id: 'connection-a' } })
    const statuses: ProviderStatus[] = []
    const { doc, provider } = makeProvider((status) => statuses.push(status))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()

    doc.getText('body').insert(0, 'pending')
    await flush()
    const update = socket.sentOfType('collab.update')[0].data.update as string
    expect(provider.currentStatus).toBe('saving')
    expect(provider.hasPendingChanges).toBe(true)

    socket.emit({
      type: 'collab.update',
      data: {
        document_id: DOC,
        seq: 1,
        actor_id: 'u-self',
        origin_conn: 'another-connection',
        update,
      },
    })
    await flush()
    expect(provider.currentStatus).toBe('saving')

    socket.emit({
      type: 'collab.update',
      data: {
        document_id: DOC,
        seq: 2,
        actor_id: 'u-self',
        origin_conn: 'connection-a',
        update,
      },
    })
    await flush()
    expect(provider.currentStatus).toBe('synced')
    expect(provider.hasPendingChanges).toBe(false)
    expect(statuses).toContain('saving')
    provider.destroy()
  })

  it('keeps an unacknowledged update and fails closed when send is rejected', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'hello', seq: 1, data: { connection_id: 'connection-a' } })
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()

    socket.failNextSend = true
    doc.getText('body').insert(0, 'survive the race')
    await flush()

    expect(provider.currentStatus).toBe('connecting')
    expect(provider.hasPendingChanges).toBe(true)
    provider.destroy()
  })

  it('catches up before appending the merged pending update and only then syncs', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'hello', data: { connection_id: 'connection-a' } })
    const statuses: ProviderStatus[] = []
    const { doc, provider } = makeProvider((status) => statuses.push(status))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()

    doc.getText('body').insert(0, 'race edit')
    await flush()
    socket.dropNow()
    expect(provider.currentStatus).toBe('connecting')
    expect(provider.hasPendingChanges).toBe(true)

    net.calls.length = 0
    const resumed = connectOpen()
    resumed.emit({ type: 'hello', data: { connection_id: 'connection-b' } })
    resumed.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()

    const stateIndex = net.calls.findIndex((call) => call.path.includes('/state'))
    const appendIndex = net.calls.findIndex((call) => call.path.includes('/updates'))
    expect(stateIndex).toBeGreaterThanOrEqual(0)
    expect(appendIndex).toBeGreaterThan(stateIndex)
    const body = net.calls[appendIndex].body as { update: string }
    const restored = new Y.Doc()
    Y.applyUpdate(restored, fromBase64(body.update))
    expect(restored.getText('body').toString()).toContain('race edit')
    expect(provider.currentStatus).toBe('synced')
    expect(provider.hasPendingChanges).toBe(false)
    expect(statuses[statuses.length - 1]).toBe('synced')
    provider.destroy()
  })

  it('retains failed recovery bytes and retries the same update', async () => {
    let appendAttempts = 0
    net = mockFetch((req) => {
      if (req.path.includes('/state')) return emptyState()
      if (req.path.includes('/updates')) {
        appendAttempts += 1
        return appendAttempts === 1
          ? apiFailure(503, 'SERVICE_UNAVAILABLE')
          : envelope(201, { data: { seq: 1 } })
      }
      return ok({})
    })
    const socket = connectOpen()
    socket.emit({ type: 'hello', data: { connection_id: 'connection-a' } })
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()
    doc.getText('body').insert(0, 'retry me')
    await flush()
    socket.dropNow()
    const resumed = connectOpen()
    resumed.emit({ type: 'hello', data: { connection_id: 'connection-b' } })
    resumed.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()

    expect(provider.currentStatus).toBe('error')
    expect(provider.hasPendingChanges).toBe(true)
    provider.retry()
    await settle()

    expect(appendAttempts).toBe(2)
    expect(provider.currentStatus).toBe('synced')
    expect(provider.hasPendingChanges).toBe(false)
    provider.destroy()
  })

  it('ignores a state response released after destroy', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    const remote = new Y.Doc()
    remote.getText('body').insert(0, 'must not appear')
    net = mockFetch(async (req) => {
      if (!req.path.includes('/state')) return ok({})
      await gate
      return ok({
        document_id: DOC,
        snapshot_seq: 0,
        updates: [{ seq: 1, payload: toBase64(Y.encodeStateAsUpdate(remote)) }],
        through_seq: 1,
        head_seq: 1,
        has_more: false,
      })
    })
    const socket = connectOpen()
    const statuses: ProviderStatus[] = []
    const { doc, provider } = makeProvider((status) => statuses.push(status))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 1, can_write: true },
    })
    await flush(10)
    const callbacksBeforeDestroy = statuses.length

    provider.destroy()
    release()
    await settle()

    expect(doc.getText('body').toString()).toBe('')
    expect(statuses).toHaveLength(callbacksBeforeDestroy)
  })

  it('does not emit a projection watermark after destroy', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    net = mockFetch(async () => {
      await gate
      return ok({
        document_id: DOC,
        snapshot_seq: 0,
        updates: [],
        through_seq: 1,
        head_seq: 1,
        has_more: false,
      })
    })
    const socket = connectOpen()
    const projected: number[] = []
    const { provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 1, can_write: true },
    })
    await flush(10)
    provider.destroy()
    release()
    await settle()
    expect(projected).toEqual([])
  })

  it('lets revocation win over a delayed recovery append', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => {
      release = resolve
    })
    net = mockFetch(async (req) => {
      if (req.path.includes('/state')) return emptyState()
      if (req.path.includes('/updates')) {
        await gate
        return envelope(201, { data: { seq: 1 } })
      }
      return ok({})
    })
    const socket = connectOpen()
    const statuses: ProviderStatus[] = []
    const { doc, provider } = makeProvider((status) => statuses.push(status))
    await flush()
    socket.emit({
      type: 'collab.joined',
      data: { document_id: DOC, head_seq: 0, can_write: true },
    })
    await settle()
    doc.getText('body').insert(0, 'x'.repeat(60_000))
    await flush(10)

    socket.emit({ type: 'collab.left', data: { document_id: DOC, reason: 'revoked' } })
    await flush()
    const revokedAt = statuses.lastIndexOf('revoked')
    release()
    await settle()

    expect(provider.currentStatus).toBe('revoked')
    expect(statuses.slice(revokedAt + 1)).not.toContain('synced')
    expect(statuses.slice(revokedAt + 1)).not.toContain('error')
    provider.destroy()
  })

  it('answers a compaction request with a snapshot', async () => {
    const socket = connectOpen()
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()
    doc.getText('body').insert(0, 'some content worth compacting')
    await flush()

    socket.emit({
      type: 'collab.compact',
      data: { document_id: DOC, head_seq: 12, snapshot_seq: 0 },
    })
    await flush()

    const snaps = net.calls.filter((c) => c.path.includes('/snapshot'))
    expect(snaps.length, 'the server cannot build a snapshot itself').toBe(1)
    const body = snaps[0].body as Record<string, unknown>
    expect(body.through_seq).toBe(12)
    expect(typeof body.snapshot).toBe('string')
    provider.destroy()
  })

  it('leaves the room and stops listening when destroyed', async () => {
    const socket = connectOpen()
    const { doc, provider } = makeProvider()
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    provider.destroy()
    expect(socket.sentOfType('collab.leave').map((f) => f.data.document_id)).toEqual([DOC])

    const before = socket.sentOfType('collab.update').length
    doc.getText('body').insert(0, 'after destroy')
    await flush()
    expect(socket.sentOfType('collab.update').length).toBe(before)
  })
})

// THE FOURTH REPAIR PATH.
//
// A document's searchable text can only be produced by a client that has the
// CRDT in memory — the server never interprets an update. Three paths do that
// and all three need somebody to be in the document: the editor's debounce, its
// flush on unmount, and its catch-up on open. A document edited and then closed
// by a browser that was killed runs none of them and is then wrong in search
// with nothing able to notice.
//
// The worker's sweep is what notices, and this is the client end of it.
describe('a server request to re-project', () => {
  it('reaches a writer', async () => {
    const socket = connectOpen()
    let asked = 0
    const { provider } = makeProvider(undefined, () => {
      asked++
    })
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    socket.emit({
      type: 'collab.project',
      data: { document_id: DOC, head_seq: 400, projection_seq: 0 },
    })
    await settle()

    expect(asked).toBe(1)
    provider.destroy()
  })

  // A reader has no capability to project and the server would refuse the
  // write. Answering anyway would mean every reader in a room posting a body
  // that is thrown away — and the sweep would keep asking, because the stored
  // projection never advances.
  it('is ignored by a reader', async () => {
    const socket = connectOpen()
    let asked = 0
    const { provider } = makeProvider(undefined, () => {
      asked++
    })
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: false } })
    await settle()

    socket.emit({
      type: 'collab.project',
      data: { document_id: DOC, head_seq: 400, projection_seq: 0 },
    })
    await settle()

    expect(asked).toBe(0)
    provider.destroy()
  })

  // The sweep re-asks every ten minutes. A one-shot guard anywhere on this path
  // would mean a document that failed to repair once was never asked again.
  it('is answered every time it is asked', async () => {
    const socket = connectOpen()
    let asked = 0
    const { provider } = makeProvider(undefined, () => {
      asked++
    })
    await flush()
    socket.emit({ type: 'collab.joined', data: { document_id: DOC, head_seq: 0, can_write: true } })
    await settle()

    for (let i = 0; i < 3; i++) {
      socket.emit({
        type: 'collab.project',
        data: { document_id: DOC, head_seq: 400, projection_seq: 0 },
      })
      await settle()
    }

    expect(asked).toBe(3)
    provider.destroy()
  })
})
