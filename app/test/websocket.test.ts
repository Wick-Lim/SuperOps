import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { wsManager, type ResyncReason } from '../src/lib/websocket'
import { useAuthStore } from '../src/stores/authStore'
import { useChannelStore } from '../src/stores/channelStore'
import { useMessageStore } from '../src/stores/messageStore'
import { useUiStore } from '../src/stores/uiStore'
import { useWorkspaceStore } from '../src/stores/workspaceStore'
import type { Workspace } from '../src/lib/types'
import {
  FakeWebSocket,
  flush,
  installFakeWebSocket,
  makeChannel,
  makeMessage,
  mockFetch,
  ok,
  resetStores,
  signIn,
  type FetchMock,
} from './helpers'

const realFetch = globalThis.fetch
let net: FetchMock = { calls: [], to: () => [] }

/** `wsManager` is a module singleton; its resync cooldown is wall-clock based. */
let clock = Date.UTC(2026, 6, 25, 12, 0, 0)

const UNREAD = '/notifications/unread-count'

function resyncCount(): number {
  return net.to(UNREAD).length
}

function connectOpen(): FakeWebSocket {
  wsManager.connect()
  const socket = FakeWebSocket.last
  socket.openNow()
  return socket
}

function channelIds(frames: { data: Record<string, unknown> }[]): unknown[] {
  return frames.map((f) => f.data.channel_id)
}

beforeEach(() => {
  vi.useFakeTimers()
  // Step the clock well past RESYNC_COOLDOWN_MS so one test's resync cannot
  // suppress the next test's.
  clock += 600_000
  vi.setSystemTime(clock)
  resetStores()
  wsManager.reset()
  installFakeWebSocket()
  signIn()
  net = mockFetch(() => ok({ count: 0 }))
})

afterEach(() => {
  wsManager.reset()
  globalThis.fetch = realFetch
  vi.useRealTimers()
})

// ---------------------------------------------------------------------------
// Lifecycle — the zombie-socket bug
// ---------------------------------------------------------------------------

describe('disconnect()', () => {
  it('closes the socket and schedules NO reconnect', async () => {
    const socket = connectOpen()

    wsManager.disconnect()

    expect(socket.closeCount).toBe(1)
    expect(wsManager.isConnected()).toBe(false)
    expect(useUiStore.getState().connection).toBe('idle')
    // A zombie socket used to come back roughly a second later.
    await vi.advanceTimersByTimeAsync(60_000)
    expect(FakeWebSocket.instances).toHaveLength(1)
    expect(vi.getTimerCount()).toBe(0)
  })

  it('cancels a reconnect that a previous unintentional close already scheduled', async () => {
    const socket = connectOpen()
    socket.dropNow()
    expect(useUiStore.getState().connection).toBe('offline')

    wsManager.disconnect()

    await vi.advanceTimersByTimeAsync(60_000)
    expect(FakeWebSocket.instances).toHaveLength(1)
    expect(useUiStore.getState().connection).toBe('idle')
  })

  it('clears typing timers and the typing indicators they own', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'typing.indicator', seq: 1, data: { channel_id: 'c1', user_id: 'u-other' } })
    socket.emit({ type: 'typing.indicator', seq: 2, data: { channel_id: 'c2', user_id: 'u-third' } })
    expect(useUiStore.getState().typing).toEqual({ c1: ['u-other'], c2: ['u-third'] })

    wsManager.disconnect()

    expect(useUiStore.getState().typing).toEqual({})
    // No orphan TTL timers left to fire against a dead connection.
    expect(vi.getTimerCount()).toBe(0)
  })

  it('makes the next connect() a first connect, not a reconnect', async () => {
    connectOpen()
    wsManager.disconnect()

    wsManager.connect()
    expect(useUiStore.getState().connection).toBe('connecting')
    FakeWebSocket.last.openNow()
    await flush(20)
    // A first connect has nothing to catch up on.
    expect(resyncCount()).toBe(0)
  })

  it('does not connect without an access token', () => {
    useAuthStore.setState({ accessToken: null })
    wsManager.connect()
    expect(FakeWebSocket.instances).toHaveLength(0)
  })

  it('puts the bearer token in the query string (the WS handshake has no headers)', () => {
    signIn('tok/9')
    wsManager.connect()
    expect(FakeWebSocket.last.url).toBe('ws://api.test/api/v1/ws?token=tok%2F9')
  })
})

describe('reconnect', () => {
  it('reconnects after an unintentional close', async () => {
    const socket = connectOpen()
    socket.dropNow()

    await vi.advanceTimersByTimeAsync(999)
    expect(FakeWebSocket.instances).toHaveLength(1)
    await vi.advanceTimersByTimeAsync(1)
    expect(FakeWebSocket.instances).toHaveLength(2)
    expect(useUiStore.getState().connection).toBe('reconnecting')
  })

  it('backs off exponentially and clamps at 30s', async () => {
    connectOpen()
    const observed: number[] = []
    let delay = 1000

    for (let i = 0; i < 8; i++) {
      const before = FakeWebSocket.instances.length
      FakeWebSocket.last.dropNow()
      await vi.advanceTimersByTimeAsync(delay - 1)
      expect(FakeWebSocket.instances).toHaveLength(before)
      await vi.advanceTimersByTimeAsync(1)
      expect(FakeWebSocket.instances).toHaveLength(before + 1)
      observed.push(delay)
      delay = Math.min(delay * 2, 30_000)
    }

    expect(observed).toEqual([1000, 2000, 4000, 8000, 16_000, 30_000, 30_000, 30_000])
  })

  it('resets the backoff once a connection opens', async () => {
    connectOpen()
    FakeWebSocket.last.dropNow()
    await vi.advanceTimersByTimeAsync(1000)
    FakeWebSocket.last.openNow() // successful reconnect resets the delay
    await flush(20)

    FakeWebSocket.last.dropNow()
    await vi.advanceTimersByTimeAsync(999)
    expect(FakeWebSocket.instances).toHaveLength(2)
    await vi.advanceTimersByTimeAsync(1)
    expect(FakeWebSocket.instances).toHaveLength(3)
  })

  it('resyncs on reconnect but not on the first connect', async () => {
    connectOpen()
    await flush(20)
    expect(resyncCount()).toBe(0)

    FakeWebSocket.last.dropNow()
    await vi.advanceTimersByTimeAsync(1000)
    FakeWebSocket.last.openNow()
    await flush(20)
    expect(resyncCount()).toBe(1)
  })
})

// ---------------------------------------------------------------------------
// Sequence gap detection
// ---------------------------------------------------------------------------

describe('seq gap detection', () => {
  it('does not resync while seqs are contiguous', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'pong', seq: 1 })
    socket.emit({ type: 'pong', seq: 2 })
    socket.emit({ type: 'pong', seq: 3 })
    await flush(20)
    expect(resyncCount()).toBe(0)
  })

  it('ignores frames with no seq at all', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'pong', seq: 1 })
    socket.emit({ type: 'pong' })
    socket.emit({ type: 'pong', seq: 2 })
    await flush(20)
    expect(resyncCount()).toBe(0)
  })

  it('resyncs exactly once for a burst of gaps, then again after the cooldown', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'pong', seq: 1 })

    socket.emit({ type: 'pong', seq: 7 }) // gap
    await flush(20)
    expect(resyncCount()).toBe(1)

    // More gaps inside RESYNC_COOLDOWN_MS must not stampede the REST API.
    socket.emit({ type: 'pong', seq: 20 })
    socket.emit({ type: 'pong', seq: 99 })
    await flush(20)
    expect(resyncCount()).toBe(1)

    await vi.advanceTimersByTimeAsync(2000)
    socket.emit({ type: 'pong', seq: 400 })
    await flush(20)
    expect(resyncCount()).toBe(2)
  })

  it('restarts the sequence on a new connection instead of seeing a false gap', async () => {
    const socket = connectOpen()
    socket.emit({ type: 'pong', seq: 42 })
    socket.dropNow()
    await vi.advanceTimersByTimeAsync(1000)
    const next = FakeWebSocket.last
    next.openNow()
    await flush(20)
    const afterReconnect = resyncCount() // the reconnect resync itself

    // Seq is per connection: the new socket starts at 1, which is not a gap.
    await vi.advanceTimersByTimeAsync(2000)
    next.emit({ type: 'pong', seq: 1 })
    next.emit({ type: 'pong', seq: 2 })
    await flush(20)
    expect(resyncCount()).toBe(afterReconnect)
  })

  it('refetches the loaded channels and the badge on resync', async () => {
    useWorkspaceStore.setState({ activeWorkspace: { id: 'ws-1' } as Workspace })
    useMessageStore.getState().setMessages('c1', [makeMessage('m1', 'c1')], 'old-cursor', true)
    net = mockFetch((req) => {
      if (req.path === '/workspaces/ws-1/channels') return ok([makeChannel('c1')])
      if (req.path.startsWith('/channels/c1/messages')) {
        return ok([makeMessage('m2', 'c1')], { cursor: 'new-cursor', has_more: false })
      }
      return ok({ count: 7 })
    })

    const seen: ResyncReason[] = []
    const off = wsManager.onResync((r) => seen.push(r))
    const socket = connectOpen()
    socket.emit({ type: 'pong', seq: 1 })
    socket.emit({ type: 'pong', seq: 5 })
    await flush(40)
    off()

    expect(seen).toEqual(['seq-gap'])
    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c1'])
    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m2'])
    expect(useMessageStore.getState().cursors['c1']).toBe('new-cursor')
    expect(useMessageStore.getState().hasMore['c1']).toBe(false)
    expect(useUiStore.getState().unreadNotifications).toBe(7)
  })
})

// ---------------------------------------------------------------------------
// Subscription acks
// ---------------------------------------------------------------------------

describe('subscriptions', () => {
  it('treats a channel as subscribed only after the server acks it', () => {
    const socket = connectOpen()
    wsManager.subscribe('c1')

    expect(channelIds(socket.sentOfType('subscribe'))).toEqual(['c1'])
    expect(wsManager.isSubscribed('c1')).toBe(false)

    socket.emit({ type: 'subscribed', seq: 1, data: { channel_id: 'c1' } })
    expect(wsManager.isSubscribed('c1')).toBe(true)
  })

  it('sends only the difference from setSubscriptions', () => {
    const socket = connectOpen()
    wsManager.setSubscriptions(['c1', 'c2'])
    const afterFirst = socket.sent.length

    wsManager.setSubscriptions(['c2', 'c3'])

    const delta = socket.sent.slice(afterFirst)
    expect(delta).toEqual([
      { type: 'unsubscribe', data: { channel_id: 'c1' } },
      { type: 'subscribe', data: { channel_id: 'c3' } },
    ])
  })

  it('re-asserts every desired channel on a new connection and drops the old acks', async () => {
    const socket = connectOpen()
    wsManager.setSubscriptions(['c1', 'c2'])
    socket.emit({ type: 'subscribed', seq: 1, data: { channel_id: 'c1' } })
    expect(wsManager.isSubscribed('c1')).toBe(true)

    socket.dropNow()
    expect(wsManager.isSubscribed('c1')).toBe(false)

    await vi.advanceTimersByTimeAsync(1000)
    const next = FakeWebSocket.last
    next.openNow()
    await flush(20)

    expect(channelIds(next.sentOfType('subscribe')).sort()).toEqual(['c1', 'c2'])
    // Acks are per connection; nothing is confirmed until the new socket says so.
    expect(wsManager.isSubscribed('c1')).toBe(false)
  })

  it('drops the channel entirely when unsubscribed carries reason=revoked', async () => {
    const socket = connectOpen()
    useChannelStore.setState({ channels: [makeChannel('c1'), makeChannel('c2')] })
    useMessageStore.getState().setMessages('c1', [makeMessage('m1', 'c1')], 'cur', true)
    wsManager.subscribe('c1')
    socket.emit({ type: 'subscribed', seq: 1, data: { channel_id: 'c1' } })
    const subscribesBefore = socket.sentOfType('subscribe').length

    socket.emit({ type: 'unsubscribed', seq: 2, data: { channel_id: 'c1', reason: 'revoked' } })
    await flush(20)

    expect(wsManager.isSubscribed('c1')).toBe(false)
    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c2'])
    expect(useMessageStore.getState().messages['c1']).toBeUndefined()
    expect(useMessageStore.getState().cursors['c1']).toBeUndefined()
    expect(resyncCount()).toBe(1)
    // Removed from the desired set too, so a later subscribe is a fresh request
    // rather than a no-op against a channel we can no longer see.
    wsManager.subscribe('c1')
    expect(socket.sentOfType('subscribe')).toHaveLength(subscribesBefore + 1)
  })

  it('keeps the channel when unsubscribed has no revoked reason', async () => {
    const socket = connectOpen()
    useChannelStore.setState({ channels: [makeChannel('c1')] })
    wsManager.subscribe('c1')
    socket.emit({ type: 'subscribed', seq: 1, data: { channel_id: 'c1' } })

    socket.emit({ type: 'unsubscribed', seq: 2, data: { channel_id: 'c1' } })
    await flush(20)

    expect(wsManager.isSubscribed('c1')).toBe(false)
    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c1'])
    expect(resyncCount()).toBe(0)
  })

  it('drops queued sends when the socket is not open', () => {
    wsManager.connect() // CONNECTING, never opened
    wsManager.subscribe('c1')
    expect(FakeWebSocket.last.sentRaw).toHaveLength(0)
  })
})

// ---------------------------------------------------------------------------
// Frame dispatch
// ---------------------------------------------------------------------------

describe('frame dispatch', () => {
  it('records the connection id and marks the UI connected on hello', () => {
    const socket = connectOpen()
    socket.emit({ type: 'hello', seq: 1, data: { connection_id: 'conn-7' } })
    expect(wsManager.getConnectionId()).toBe('conn-7')
    expect(useUiStore.getState().connection).toBe('connected')
  })

  it('surfaces an error frame code without dropping the connection', () => {
    const socket = connectOpen()
    const warn = vi.spyOn(console, 'warn').mockImplementation(() => {})
    socket.emit({ type: 'error', seq: 1, data: { code: 'FORBIDDEN', message: 'not a member' } })
    expect(useUiStore.getState().connectionError).toBe('FORBIDDEN')
    expect(useUiStore.getState().connection).toBe('connected')
    expect(wsManager.isConnected()).toBe(true)
    warn.mockRestore()
  })

  it('appends a message.new, normalizing a partial worker payload', () => {
    const socket = connectOpen()
    socket.emit({
      type: 'message.new',
      seq: 1,
      data: {
        id: 'm1',
        channel_id: 'c1',
        user_id: 'u-other',
        content: 'from the scheduler',
        created_at: '2026-07-25T01:00:00Z',
        parent_id: null,
      },
    })

    const list = useMessageStore.getState().messages['c1']
    expect(list.map((m) => m.id)).toEqual(['m1'])
    expect(list[0].content_type).toBe('text')
    expect(list[0].reactions).toBeNull()
    expect(list[0].metadata).toEqual({})
    expect(list[0].updated_at).toBe('2026-07-25T01:00:00Z')
  })

  it('replaces in place on message.updated and ignores one outside the window', () => {
    useMessageStore
      .getState()
      .setMessages('c1', [makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], '', false)
    const socket = connectOpen()

    socket.emit({
      type: 'message.updated',
      seq: 1,
      data: { ...makeMessage('m1', 'c1'), content: 'edited', is_edited: true },
    })
    socket.emit({
      type: 'message.updated',
      seq: 2,
      data: { ...makeMessage('m-old', 'c1'), content: 'ancient edit' },
    })

    const list = useMessageStore.getState().messages['c1']
    // m1 stays first (setMessages reversed the newest-first page) and the
    // out-of-window edit did NOT get appended to the bottom.
    expect(list.map((m) => m.id)).toEqual(['m1', 'm2'])
    expect(list[0].content).toBe('edited')
  })

  it('removes a message on message.deleted under either id key', () => {
    useMessageStore
      .getState()
      .setMessages('c1', [makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], '', false)
    const socket = connectOpen()

    socket.emit({ type: 'message.deleted', seq: 1, data: { channel_id: 'c1', id: 'm1' } })
    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m2'])

    socket.emit({ type: 'message.deleted', seq: 2, data: { channel_id: 'c1', message_id: 'm2' } })
    expect(useMessageStore.getState().messages['c1']).toEqual([])
  })

  it('applies and removes reactions', () => {
    useMessageStore.getState().setMessages('c1', [makeMessage('m1', 'c1')], '', false)
    const socket = connectOpen()

    socket.emit({
      type: 'reaction.added',
      seq: 1,
      data: { channel_id: 'c1', message_id: 'm1', user_id: 'u-other', emoji: '🎉' },
    })
    expect(useMessageStore.getState().messages['c1'][0].reactions).toHaveLength(1)

    // A duplicate add must not double-count.
    socket.emit({
      type: 'reaction.added',
      seq: 2,
      data: { channel_id: 'c1', message_id: 'm1', user_id: 'u-other', emoji: '🎉' },
    })
    expect(useMessageStore.getState().messages['c1'][0].reactions).toHaveLength(1)

    socket.emit({
      type: 'reaction.removed',
      seq: 3,
      data: { channel_id: 'c1', message_id: 'm1', user_id: 'u-other', emoji: '🎉' },
    })
    expect(useMessageStore.getState().messages['c1'][0].reactions).toEqual([])
  })

  it('expires a typing indicator after its TTL and clears it early on typing:false', async () => {
    const socket = connectOpen()

    socket.emit({ type: 'typing.indicator', seq: 1, data: { channel_id: 'c1', user_id: 'u-other' } })
    expect(useUiStore.getState().typing.c1).toEqual(['u-other'])
    await vi.advanceTimersByTimeAsync(3999)
    expect(useUiStore.getState().typing.c1).toEqual(['u-other'])
    await vi.advanceTimersByTimeAsync(1)
    expect(useUiStore.getState().typing.c1).toEqual([])

    socket.emit({ type: 'typing.indicator', seq: 2, data: { channel_id: 'c1', user_id: 'u-other' } })
    expect(useUiStore.getState().typing.c1).toEqual(['u-other'])
    socket.emit({
      type: 'typing.indicator',
      seq: 3,
      data: { channel_id: 'c1', user_id: 'u-other', typing: false },
    })
    expect(useUiStore.getState().typing.c1).toEqual([])
    expect(vi.getTimerCount()).toBe(0)
  })

  it('ignores its own typing indicator', () => {
    const socket = connectOpen()
    socket.emit({ type: 'typing.indicator', seq: 1, data: { channel_id: 'c1', user_id: 'u-self' } })
    expect(useUiStore.getState().typing).toEqual({})
  })

  it('tracks presence and the unread badge', () => {
    const socket = connectOpen()
    socket.emit({ type: 'presence.changed', seq: 1, data: { user_id: 'u-other', status: 'away' } })
    expect(useUiStore.getState().presence['u-other']).toBe('away')

    socket.emit({ type: 'notification.new', seq: 2, data: { id: 'n1' } })
    socket.emit({ type: 'notification.new', seq: 3, data: { id: 'n2' } })
    expect(useUiStore.getState().unreadNotifications).toBe(2)

    useChannelStore.setState({ channels: [makeChannel('c1')] })
    socket.emit({ type: 'unread.update', seq: 4, data: { channel_id: 'c1', unread_count: 5 } })
    expect(useChannelStore.getState().channels[0].unread_count).toBe(5)
  })

  it('adds a created channel and only auto-subscribes to non-public ones', () => {
    useWorkspaceStore.setState({ activeWorkspace: { id: 'ws-1' } as Workspace })
    const socket = connectOpen()

    socket.emit({ type: 'channel.created', seq: 1, data: makeChannel('c-pub', { type: 'public' }) })
    socket.emit({
      type: 'channel.created',
      seq: 2,
      data: { channel: makeChannel('c-dm', { type: 'dm', name: null }) },
    })
    // Another workspace's announcement must not leak into this sidebar.
    socket.emit({
      type: 'channel.created',
      seq: 3,
      data: makeChannel('c-other', { workspace_id: 'ws-2' }),
    })

    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c-pub', 'c-dm'])
    expect(channelIds(socket.sentOfType('subscribe'))).toEqual(['c-dm'])
  })

  it('unsubscribes when a channel is archived', () => {
    const socket = connectOpen()
    useChannelStore.setState({ channels: [makeChannel('c1')] })
    wsManager.subscribe('c1')

    socket.emit({ type: 'channel.updated', seq: 1, data: makeChannel('c1', { is_archived: true }) })

    expect(useChannelStore.getState().channels[0].is_archived).toBe(true)
    expect(channelIds(socket.sentOfType('unsubscribe'))).toEqual(['c1'])
  })

  it('subscribes on my own member.joined and tears down on my own member.left', async () => {
    const socket = connectOpen()
    useChannelStore.setState({ channels: [makeChannel('c1')] })
    useMessageStore.getState().setMessages('c1', [makeMessage('m1', 'c1')], 'cur', true)

    socket.emit({ type: 'member.joined', seq: 1, data: { channel_id: 'c1', user_id: 'u-other' } })
    expect(socket.sentOfType('subscribe')).toHaveLength(0)

    socket.emit({ type: 'member.joined', seq: 2, data: { channel_id: 'c1', user_id: 'u-self' } })
    expect(channelIds(socket.sentOfType('subscribe'))).toEqual(['c1'])

    socket.emit({ type: 'member.left', seq: 3, data: { channel_id: 'c1', user_id: 'u-self' } })
    await flush(20)
    expect(channelIds(socket.sentOfType('unsubscribe'))).toEqual(['c1'])
    expect(useChannelStore.getState().channels).toEqual([])
    expect(useMessageStore.getState().messages['c1']).toBeUndefined()
  })

  it('ignores malformed frames instead of throwing', () => {
    const socket = connectOpen()
    expect(() => socket.emitRaw('<html>502</html>')).not.toThrow()
    expect(() => socket.emitRaw('null')).not.toThrow()
    expect(() => socket.emit({ type: '' } as never)).not.toThrow()
    expect(() => socket.emitRaw(JSON.stringify({ seq: 1 }))).not.toThrow()
    expect(wsManager.isConnected()).toBe(true)
  })

  it('fans a frame out to registered handlers until they unsubscribe', () => {
    const socket = connectOpen()
    const seen: unknown[] = []
    const off = wsManager.on('message.new', (d) => seen.push(d))

    const payload = {
      id: 'm1',
      channel_id: 'c1',
      user_id: 'u-other',
      content: 'hi',
      created_at: '2026-07-25T01:00:00Z',
    }
    socket.emit({ type: 'message.new', seq: 1, data: payload })
    off()
    socket.emit({ type: 'message.new', seq: 2, data: { ...payload, id: 'm2' } })

    expect(seen).toEqual([payload])
  })
})
