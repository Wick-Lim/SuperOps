import { beforeEach, describe, expect, it } from 'vitest'
import { useChannelStore } from '../src/stores/channelStore'
import { useMessageStore } from '../src/stores/messageStore'
import { useUiStore } from '../src/stores/uiStore'
import type { Reaction } from '../src/lib/types'
import { makeChannel, makeMessage, resetStores } from './helpers'

beforeEach(() => {
  resetStores()
})

function reaction(messageId: string, userId: string, emoji: string): Reaction {
  return { id: '', message_id: messageId, user_id: userId, emoji, created_at: '' }
}

// ---------------------------------------------------------------------------
// messageStore
// ---------------------------------------------------------------------------

describe('messageStore ordering', () => {
  it('reverses a newest-first page into the ascending render order', () => {
    // The API answers newest first; the list renders oldest -> newest.
    useMessageStore
      .getState()
      .setMessages('c1', [makeMessage('m3', 'c1'), makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], 'cur', true)

    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m1', 'm2', 'm3'])
    expect(useMessageStore.getState().cursors['c1']).toBe('cur')
    expect(useMessageStore.getState().hasMore['c1']).toBe(true)
  })

  it('puts an older page in front of the live window', () => {
    const ms = useMessageStore.getState()
    ms.setMessages('c1', [makeMessage('m4', 'c1'), makeMessage('m3', 'c1')], 'cur1', true)
    ms.prependMessages('c1', [makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], 'cur2', false)

    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m1', 'm2', 'm3', 'm4'])
    expect(useMessageStore.getState().cursors['c1']).toBe('cur2')
    expect(useMessageStore.getState().hasMore['c1']).toBe(false)
  })

  it('appends a new message but upserts a known id IN PLACE', () => {
    const ms = useMessageStore.getState()
    ms.setMessages('c1', [makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], '', false)

    ms.addMessage('c1', makeMessage('m3', 'c1'))
    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m1', 'm2', 'm3'])

    // A redelivered / echoed message must not jump to the bottom or duplicate.
    ms.addMessage('c1', makeMessage('m1', 'c1', { content: 'echo' }))
    const list = useMessageStore.getState().messages['c1']
    expect(list.map((m) => m.id)).toEqual(['m1', 'm2', 'm3'])
    expect(list[0].content).toBe('echo')
  })

  it('starts a channel that has no loaded window', () => {
    useMessageStore.getState().addMessage('c-new', makeMessage('m1', 'c-new'))
    expect(useMessageStore.getState().messages['c-new'].map((m) => m.id)).toEqual(['m1'])
  })

  it('caps the live window at 500 and invalidates the now-bogus cursor', () => {
    const page = Array.from({ length: 500 }, (_, i) => makeMessage(`m${500 - i}`, 'c1'))
    useMessageStore.getState().setMessages('c1', page, 'cur', true)
    expect(useMessageStore.getState().messages['c1']).toHaveLength(500)
    expect(useMessageStore.getState().cursors['c1']).toBe('cur')

    useMessageStore.getState().addMessage('c1', makeMessage('m501', 'c1'))

    const list = useMessageStore.getState().messages['c1']
    expect(list).toHaveLength(500)
    expect(list[0].id).toBe('m2')
    expect(list[499].id).toBe('m501')
    // Paging with the retained cursor would splice a hole into the history.
    expect(useMessageStore.getState().cursors['c1']).toBe('')
    expect(useMessageStore.getState().hasMore['c1']).toBe(true)
  })

  it('updateMessage is replace-only — an out-of-window edit is dropped, not appended', () => {
    const ms = useMessageStore.getState()
    ms.setMessages('c1', [makeMessage('m2', 'c1'), makeMessage('m1', 'c1')], '', false)

    ms.updateMessage('c1', makeMessage('m-scrolled-away', 'c1', { content: 'ancient' }))

    expect(useMessageStore.getState().messages['c1'].map((m) => m.id)).toEqual(['m1', 'm2'])
  })

  it('leaves state untouched when removing an unknown message', () => {
    useMessageStore.getState().setMessages('c1', [makeMessage('m1', 'c1')], '', false)
    const before = useMessageStore.getState().messages['c1']

    useMessageStore.getState().removeMessage('c1', 'nope')
    expect(useMessageStore.getState().messages['c1']).toBe(before)

    useMessageStore.getState().removeMessage('c-unknown', 'm1')
    expect(useMessageStore.getState().messages['c-unknown']).toBeUndefined()
  })

  it('dedupes reactions per (user, emoji) and removes them again', () => {
    const ms = useMessageStore.getState()
    ms.setMessages('c1', [makeMessage('m1', 'c1')], '', false)

    ms.applyReaction('c1', reaction('m1', 'u1', '👍'), true)
    ms.applyReaction('c1', reaction('m1', 'u1', '👍'), true)
    ms.applyReaction('c1', reaction('m1', 'u2', '👍'), true)
    expect(useMessageStore.getState().messages['c1'][0].reactions).toHaveLength(2)

    ms.applyReaction('c1', reaction('m1', 'u1', '👍'), false)
    expect(
      (useMessageStore.getState().messages['c1'][0].reactions ?? []).map((r) => r.user_id),
    ).toEqual(['u2'])

    // A reaction on a message outside the window is a no-op.
    const before = useMessageStore.getState().messages['c1']
    ms.applyReaction('c1', reaction('m-other', 'u1', '👍'), true)
    expect(useMessageStore.getState().messages['c1']).toBe(before)
  })

  it('clearChannel drops the cursor and hasMore entries alongside the messages', () => {
    const ms = useMessageStore.getState()
    ms.setMessages('c1', [makeMessage('m1', 'c1')], 'cur', true)
    ms.setMessages('c2', [makeMessage('m2', 'c2')], 'cur2', true)

    ms.clearChannel('c1')

    const s = useMessageStore.getState()
    expect(s.messages['c1']).toBeUndefined()
    expect('c1' in s.cursors).toBe(false)
    expect('c1' in s.hasMore).toBe(false)
    // The other channel is untouched.
    expect(s.messages['c2']).toHaveLength(1)
    expect(s.cursors['c2']).toBe('cur2')
  })

  it('trimChannel keeps the newest N and invalidates the cursor', () => {
    const page = Array.from({ length: 150 }, (_, i) => makeMessage(`m${150 - i}`, 'c1'))
    useMessageStore.getState().setMessages('c1', page, 'cur', false)

    useMessageStore.getState().trimChannel('c1')

    const s = useMessageStore.getState()
    expect(s.messages['c1']).toHaveLength(100)
    expect(s.messages['c1'][0].id).toBe('m51')
    expect(s.cursors['c1']).toBe('')
    expect(s.hasMore['c1']).toBe(true)

    // Below the threshold it is a no-op.
    const before = useMessageStore.getState().messages['c1']
    useMessageStore.getState().trimChannel('c1', 500)
    expect(useMessageStore.getState().messages['c1']).toBe(before)
  })
})

// ---------------------------------------------------------------------------
// channelStore
// ---------------------------------------------------------------------------

describe('channelStore', () => {
  it('dedupes a re-created DM instead of adding a second sidebar row', () => {
    const cs = useChannelStore.getState()
    const dm = makeChannel('dm-1', { type: 'dm', name: null })
    cs.setChannels([makeChannel('c1'), dm])

    // POST /channels/dm is idempotent and answers 200 with the EXISTING channel.
    cs.addChannel({ ...dm, topic: 'refreshed' })

    const channels = useChannelStore.getState().channels
    expect(channels.map((c) => c.id)).toEqual(['c1', 'dm-1'])
    expect(channels[1].topic).toBe('refreshed')
  })

  it('merges rather than replaces, so a computed unread_count survives', () => {
    const cs = useChannelStore.getState()
    cs.setChannels([makeChannel('c1', { unread_count: 4 })])

    // The DM-create / browse payloads always carry unread_count: 0.
    cs.addChannel(makeChannel('c1', { unread_count: 0, topic: 'new topic' }))

    const [c] = useChannelStore.getState().channels
    expect(c.topic).toBe('new topic')
    expect(c.unread_count).toBe(0)
    expect(useChannelStore.getState().channels).toHaveLength(1)
  })

  it('keeps activeChannel pointed at the refreshed object after setChannels', () => {
    const cs = useChannelStore.getState()
    const c1 = makeChannel('c1', { name: 'old' })
    cs.setChannels([c1])
    cs.setActiveChannel(c1)

    cs.setChannels([makeChannel('c1', { name: 'renamed' })])
    expect(useChannelStore.getState().activeChannel?.name).toBe('renamed')

    // A refresh that no longer lists the open channel keeps the stale object
    // rather than blanking the screen mid-read.
    cs.setChannels([makeChannel('c2')])
    expect(useChannelStore.getState().activeChannel?.id).toBe('c1')
  })

  it('syncs activeChannel through upsertChannel', () => {
    const cs = useChannelStore.getState()
    const c1 = makeChannel('c1')
    cs.setChannels([c1])
    cs.setActiveChannel(c1)

    cs.upsertChannel(makeChannel('c1', { topic: 'live update' }))
    expect(useChannelStore.getState().activeChannel?.topic).toBe('live update')

    cs.upsertChannel(makeChannel('c2', { topic: 'somewhere else' }))
    expect(useChannelStore.getState().activeChannel?.id).toBe('c1')
    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c1', 'c2'])
  })

  it('clears activeChannel when the open channel is removed', () => {
    const cs = useChannelStore.getState()
    const c1 = makeChannel('c1')
    cs.setChannels([c1, makeChannel('c2')])
    cs.setActiveChannel(c1)

    cs.removeChannel('c1')
    expect(useChannelStore.getState().channels.map((c) => c.id)).toEqual(['c2'])
    expect(useChannelStore.getState().activeChannel).toBeNull()

    // Removing something already gone must not disturb the list.
    const before = useChannelStore.getState().channels
    cs.removeChannel('c-gone')
    expect(useChannelStore.getState().channels).toBe(before)
  })

  it('skips the update when the unread count has not changed', () => {
    const cs = useChannelStore.getState()
    cs.setChannels([makeChannel('c1', { unread_count: 3 })])
    const before = useChannelStore.getState().channels

    cs.setUnreadCount('c1', 3)
    expect(useChannelStore.getState().channels).toBe(before)

    cs.setUnreadCount('c1', 9)
    expect(useChannelStore.getState().channels).not.toBe(before)
    expect(useChannelStore.getState().channels[0].unread_count).toBe(9)

    cs.setUnreadCount('c-unknown', 1)
    expect(useChannelStore.getState().channels).toHaveLength(1)
  })
})

// ---------------------------------------------------------------------------
// uiStore
// ---------------------------------------------------------------------------

describe('uiStore typing', () => {
  it('dedupes a user and removes only that user', () => {
    const ui = useUiStore.getState()
    ui.addTyping('c1', 'u1')
    ui.addTyping('c1', 'u1')
    ui.addTyping('c1', 'u2')
    expect(useUiStore.getState().typing.c1).toEqual(['u1', 'u2'])

    ui.removeTyping('c1', 'u1')
    expect(useUiStore.getState().typing.c1).toEqual(['u2'])

    const before = useUiStore.getState().typing
    ui.removeTyping('c1', 'u-not-typing')
    expect(useUiStore.getState().typing).toBe(before)
  })

  it('clears one channel or all of them', () => {
    const ui = useUiStore.getState()
    ui.addTyping('c1', 'u1')
    ui.addTyping('c2', 'u2')

    ui.clearTyping('c1')
    expect(Object.keys(useUiStore.getState().typing)).toEqual(['c2'])

    ui.clearTyping()
    expect(useUiStore.getState().typing).toEqual({})

    const before = useUiStore.getState().typing
    ui.clearTyping()
    expect(useUiStore.getState().typing).toBe(before)
  })

  it('resets the connection error when the status changes', () => {
    const ui = useUiStore.getState()
    ui.setConnection('connected', 'FORBIDDEN')
    expect(useUiStore.getState().connectionError).toBe('FORBIDDEN')

    ui.setConnection('connected')
    expect(useUiStore.getState().connectionError).toBeNull()
  })
})
