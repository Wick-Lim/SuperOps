import { describe, it, expect, beforeEach } from 'vitest'
import { useMessageStore } from '../messageStore'
import type { Message } from '@/lib/types'

const mockMessage = (id: string, channelId: string): Message => ({
  id, channel_id: channelId, user_id: 'u1', parent_id: null,
  content: `Message ${id}`, content_type: 'markdown',
  is_edited: false, is_deleted: false, reply_count: 0,
  created_at: new Date().toISOString(), updated_at: new Date().toISOString(),
})

describe('messageStore', () => {
  beforeEach(() => {
    useMessageStore.setState({ messages: {}, cursors: {}, hasMore: {} })
  })

  it('should set messages for a channel', () => {
    const msgs = [mockMessage('m1', 'c1'), mockMessage('m2', 'c1')]
    useMessageStore.getState().setMessages('c1', msgs, 'cursor1', true)

    const state = useMessageStore.getState()
    expect(state.messages['c1']).toHaveLength(2)
    expect(state.cursors['c1']).toBe('cursor1')
    expect(state.hasMore['c1']).toBe(true)
  })

  it('should add a message to a channel', () => {
    useMessageStore.getState().setMessages('c1', [], '', false)
    useMessageStore.getState().addMessage('c1', mockMessage('m1', 'c1'))

    expect(useMessageStore.getState().messages['c1']).toHaveLength(1)
    expect(useMessageStore.getState().messages['c1'][0].id).toBe('m1')
  })

  it('should remove a message', () => {
    const msgs = [mockMessage('m1', 'c1'), mockMessage('m2', 'c1')]
    useMessageStore.getState().setMessages('c1', msgs, '', false)
    useMessageStore.getState().removeMessage('c1', 'm1')

    const remaining = useMessageStore.getState().messages['c1']
    expect(remaining).toHaveLength(1)
    expect(remaining[0].id).toBe('m2')
  })

  it('should update a message', () => {
    const msg = mockMessage('m1', 'c1')
    useMessageStore.getState().setMessages('c1', [msg], '', false)

    const updated = { ...msg, content: 'Updated!', is_edited: true }
    useMessageStore.getState().updateMessage('c1', updated)

    expect(useMessageStore.getState().messages['c1'][0].content).toBe('Updated!')
    expect(useMessageStore.getState().messages['c1'][0].is_edited).toBe(true)
  })
})
