import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { wsManager, type HuddleEvent } from '../src/lib/websocket'
import {
  FakeWebSocket, flush, installFakeWebSocket, mockFetch, ok, resetStores, signIn, type FetchMock,
} from './helpers'

const realFetch = globalThis.fetch
let net: FetchMock = { calls: [], to: () => [] }

function connectOpen(): FakeWebSocket {
  wsManager.connect()
  const s = FakeWebSocket.last
  s.openNow()
  return s
}

beforeEach(() => {
  resetStores()
  installFakeWebSocket()
  signIn()
  net = mockFetch(() => ok({}))
})

afterEach(() => {
  wsManager.reset()
  globalThis.fetch = realFetch
})

// THE SERVER HAS PUBLISHED THESE SINCE HUDDLES SHIPPED AND THE CLIENT DROPPED
// THEM. The dispatch switch is closed, so an unhandled type reaches nothing —
// a call could start in a channel you were looking at and the bar would not
// move until you navigated away and back.
describe('huddle frames', () => {
  it('delivers started, ended and roster to a listener', async () => {
    const socket = connectOpen()
    const seen: HuddleEvent[] = []
    const off = wsManager.onHuddle((e) => seen.push(e))

    socket.emit({
      type: 'huddle.started',
      data: { channel_id: 'c1', huddle_id: 'h1', started_by: 'u1' },
    })
    socket.emit({ type: 'huddle.roster', data: { channel_id: 'c1', huddle_id: 'h1' } })
    socket.emit({
      type: 'huddle.ended',
      data: { channel_id: 'c1', huddle_id: 'h1', reason: 'empty' },
    })
    await flush()

    // Emission order, not sorted: the bar reacts to each in turn.
    expect(seen.map((e) => e.kind)).toEqual(['started', 'roster', 'ended'])
    off()
  })

  it('ignores a frame with no channel, rather than emitting a useless one', async () => {
    const socket = connectOpen()
    const seen: HuddleEvent[] = []
    wsManager.onHuddle((e) => seen.push(e))
    socket.emit({ type: 'huddle.started', data: { huddle_id: 'h1' } })
    await flush()
    expect(seen).toHaveLength(0)
  })

  it('stops delivering after the listener unsubscribes', async () => {
    const socket = connectOpen()
    const seen: HuddleEvent[] = []
    const off = wsManager.onHuddle((e) => seen.push(e))
    off()
    socket.emit({ type: 'huddle.started', data: { channel_id: 'c1', huddle_id: 'h1' } })
    await flush()
    expect(seen).toHaveLength(0)
  })
})

describe('the workflow api', () => {
  it('reads the step catalogue from the server rather than a constant', async () => {
    const { workflowApi } = await import('../src/api/workflows')
    net = mockFetch((req) => {
      if (req.path === '/workflow-steps') {
        return ok({ steps: [{ kind: 'post_message', display_name: 'Post', fields: ['channel_id'] }], triggers: ['message.created'] })
      }
      return ok({})
    })
    const res = await workflowApi.catalogue()
    // A hardcoded list would offer steps this deployment has no adapter for,
    // and the user would find out when a run failed.
    expect(res.data?.steps[0].kind).toBe('post_message')
    expect(res.data?.triggers).toContain('message.created')
    expect(net.calls.some((c) => c.path === '/workflow-steps')).toBe(true)
  })

  it('archives through DELETE, which the server also disables', async () => {
    const { workflowApi } = await import('../src/api/workflows')
    await workflowApi.archive('wf-1')
    const call = net.calls.find((c) => c.path === '/workflows/wf-1')
    expect(call?.method).toBe('DELETE')
  })
})

describe('the mailbox api', () => {
  it('sends a reply to the conversation, not to a message', async () => {
    const { mailboxApi } = await import('../src/api/mailboxes')
    await mailboxApi.reply('conv-1', 'we are on it')
    const call = net.calls.find((c) => c.path === '/conversations/conv-1/reply')
    expect(call?.method).toBe('POST')
    expect((call?.body as Record<string, unknown>)?.body_text).toBe('we are on it')
  })

  it('asks for one mailbox worth of conversations, filtered by state', async () => {
    const { mailboxApi } = await import('../src/api/mailboxes')
    await mailboxApi.conversations('mb-1', 'open')
    // One mailbox at a time is the permission boundary: an agent is granted on
    // a mailbox, so a merged list would have contents the reader cannot explain.
    expect(net.calls.some((c) => c.path.startsWith('/mailboxes/mb-1/conversations'))).toBe(true)
    expect(net.calls.some((c) => c.path.includes('state=open'))).toBe(true)
  })
})

describe('the huddle api', () => {
  it('treats a 404 as "this deployment has no media server"', async () => {
    const { huddlesAvailable } = await import('../src/api/huddles')
    net = mockFetch(() => new Response('{}', { status: 404, headers: { 'content-type': 'application/json' } }))
    // Not an error to show anybody: the routes are simply not registered.
    expect(await huddlesAvailable('c1')).toBe(false)
  })

  it('treats a successful answer as available', async () => {
    const { huddlesAvailable } = await import('../src/api/huddles')
    net = mockFetch(() => ok({ huddle: null }))
    expect(await huddlesAvailable('c1')).toBe(true)
  })
})
