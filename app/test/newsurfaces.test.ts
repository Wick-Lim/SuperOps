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

// A NOTIFICATION'S DEEP LINK SURVIVES THE SHAPE IT ARRIVES IN.
//
// `data` used to be a JSON string — the repository's `data::text` cast — and
// the inbox compat layer now emits a real object. The screen called JSON.parse
// on it, which coerces an object to "[object Object]" and throws, so every
// notification resolved to null: tapping one marked it read and navigated
// nowhere. The server's comment claimed the client parsed it either way.
describe('a notification deep link', () => {
  it('reads the channel from both encodings', async () => {
    const mod = await import('../src/screens/NotificationsScreen')
    const parse = (mod as unknown as { __parseChannelId: (d: unknown) => string | null })
      .__parseChannelId
    expect(parse, 'NotificationsScreen no longer exports its parser for testing').toBeTypeOf(
      'function',
    )

    // The shape the server sends today.
    expect(parse({ channel_id: 'c-1' })).toBe('c-1')
    // The shape an older server sends.
    expect(parse(JSON.stringify({ channel_id: 'c-1' }))).toBe('c-1')
    // And nothing usable stays null rather than throwing.
    expect(parse(null)).toBeNull()
    expect(parse('not json')).toBeNull()
    expect(parse({ channel_id: '' })).toBeNull()
    expect(parse({})).toBeNull()
  })
})

// A SEARCH HIT IS NOT ALWAYS A MESSAGE.
//
// The server returns six hit types and reads an absent `type` parameter as
// EVERY type. The client declared only the message fields and never sent one,
// so Drive documents came back and rendered as "Message from <uploader> in
// #other channel" with the document body as the message text — and tapping one
// asked for a channel whose id was the empty string, dead-ending in "That
// channel is no longer available."
describe('search hits', () => {
  it('tells a Drive object apart from a message', async () => {
    const { isDriveHit } = await import('../src/api/search')
    const base = {
      id: 'x',
      channel_id: '',
      workspace_id: 'w',
      user_id: 'u',
      content: 'body',
      created_at: 0,
    }
    expect(isDriveHit({ ...base, type: 'document' })).toBe(true)
    expect(isDriveHit({ ...base, type: 'spreadsheet' })).toBe(true)
    expect(isDriveHit({ ...base, type: 'design' })).toBe(true)
    expect(isDriveHit({ ...base, type: 'file' })).toBe(true)
    expect(isDriveHit({ ...base, type: 'message', channel_id: 'c' })).toBe(false)
    // An older server sends no type at all; treating that as a Drive object
    // would send every message to the file screen.
    expect(isDriveHit(base)).toBe(false)
    // An issue is not in Drive either — it has its own surface.
    expect(isDriveHit({ ...base, type: 'issue' })).toBe(false)
  })

  it('narrows by type only when asked', async () => {
    const { searchApi } = await import('../src/api/search')
    net = mockFetch(() => ok({ hits: [], estimated_total: 0, processing_time_ms: 1 }))

    await searchApi.messages('w-1', 'q')
    expect(net.calls[0].path).not.toContain('type=')

    await searchApi.messages('w-1', 'q', { types: ['message'] })
    expect(net.calls[1].path).toContain('type=message')
  })
})

// THE BOARD LOADED ONE PAGE OF A RANK-ORDERED LIST.
//
// issueApi.issues() returns the server's default 50 and BoardScreen read
// data.issues alone, so a project with more than 50 cards silently lost the
// rest — and because the page is ordered by rank ACROSS all states, whole
// columns came back empty while each header printed its own length as the
// total.
describe('the issue cursor', () => {
  it('follows every page', async () => {
    const { issueApi } = await import('../src/api/issues')
    const pages = [
      { issues: [{ id: 'i1' }, { id: 'i2' }], next: { after_rank: 'r2', after_id: 'i2' }, has_more: true },
      { issues: [{ id: 'i3' }], next: { after_rank: 'r3', after_id: 'i3' }, has_more: false },
    ]
    let n = 0
    net = mockFetch(() => ok(pages[n++]))

    const res = await issueApi.allIssues('p-1')
    expect(res.issues.map((i) => i.id)).toEqual(['i1', 'i2', 'i3'])
    expect(res.truncated).toBe(false)
    // The second request must carry the cursor, or it re-reads page one forever.
    expect(net.calls[1].path).toContain('after_id=i2')
  })

  it('stops at the page cap and says so, rather than looping', async () => {
    const { issueApi } = await import('../src/api/issues')
    // A server that always claims more: without a cap this never returns.
    net = mockFetch(() =>
      ok({ issues: [{ id: 'x' }], next: { after_rank: 'r', after_id: 'x' }, has_more: true }),
    )

    const res = await issueApi.allIssues('p-1', { maxPages: 3 })
    expect(res.issues).toHaveLength(3)
    expect(res.truncated).toBe(true)
  })

  it('makes exactly one request when there is nothing more', async () => {
    const { issueApi } = await import('../src/api/issues')
    net = mockFetch(() => ok({ issues: [], next: { after_rank: '', after_id: '' }, has_more: false }))

    const res = await issueApi.allIssues('p-1')
    expect(res.issues).toHaveLength(0)
    expect(net.calls).toHaveLength(1)
  })
})

// ONE PAGE IS NOT THE LIST.
//
// The shared inbox stopped at 50 conversations, a comment thread printed its
// page length as "N comments", and a workflow's run history simply ended. None
// of them read `meta`. collectPages is the one walk they now share; a copy per
// screen is how three of them drifted apart in the first place.
describe('collectPages', () => {
  it('follows the cursor to the end', async () => {
    const { collectPages } = await import('../src/api/client')
    const pages = [
      { data: [1, 2], meta: { cursor: 'c1', has_more: true } },
      { data: [3], meta: { has_more: false } },
    ]
    const seen: (string | undefined)[] = []
    let n = 0
    const out = await collectPages<number>(async (cursor) => {
      seen.push(cursor)
      return pages[n++] as never
    })
    expect(out.items).toEqual([1, 2, 3])
    expect(out.truncated).toBe(false)
    // The second call must carry the cursor, or it re-reads page one forever.
    expect(seen).toEqual([undefined, 'c1'])
  })

  it('stops at the cap against a server that always claims more', async () => {
    const { collectPages } = await import('../src/api/client')
    const out = await collectPages<number>(
      async () => ({ data: [1], meta: { cursor: 'c', has_more: true } }) as never,
      3,
    )
    expect(out.items).toHaveLength(3)
    expect(out.truncated).toBe(true)
  })

  // has_more with no cursor is a server that cannot actually continue. Treating
  // it as "keep going" is the infinite loop the cap exists to bound; treating it
  // as done is the honest reading.
  it('stops when has_more carries no cursor', async () => {
    const { collectPages } = await import('../src/api/client')
    let calls = 0
    const out = await collectPages<number>(async () => {
      calls++
      return { data: [1], meta: { has_more: true } } as never
    })
    expect(calls).toBe(1)
    expect(out.truncated).toBe(false)
  })
})
