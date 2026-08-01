import { afterEach, beforeEach, describe, expect, it } from 'vitest'
import { AccountCache } from '../src/lib/accountCache'
import {
  clearDMRosterCache,
  getCachedDMRoster,
  loadDMRoster,
} from '../src/components/channel/dmRosterCache'
import {
  clearCustomEmojiCache,
  getCustomEmojiSnapshot,
  loadCustomEmoji,
} from '../src/components/message/customEmoji'
import {
  clearWorkspaceRoleCache,
  getCachedWorkspaceRole,
  loadWorkspaceRole,
} from '../src/screens/internal/useWorkspaceRole'
import { flush, mockFetch, ok, resetStores } from './helpers'

const realFetch = globalThis.fetch

beforeEach(() => resetStores())
afterEach(() => { globalThis.fetch = realFetch })

describe('generational account caches', () => {
  it('rejects a write holding a generation from before clear', () => {
    const cache = new AccountCache<string, string>()
    const oldGeneration = cache.generation
    cache.clear()
    expect(cache.setIfCurrent(oldGeneration, 'workspace-a', 'old')).toBe(false)
    expect(cache.get('workspace-a')).toBeUndefined()
  })

  it('does not repopulate a cleared custom emoji cache from an old request', async () => {
    let release!: () => void
    mockFetch(async () => {
      await new Promise<void>((resolve) => { release = resolve })
      return ok([{ id: 'emoji-a', name: 'old', image_url: '/old.png' }])
    })

    loadCustomEmoji('workspace-a')
    await flush()
    clearCustomEmojiCache()
    release()
    await flush(20)

    expect(getCustomEmojiSnapshot('workspace-a')).toEqual([])
  })

  it('does not repopulate DM roster or workspace role after clear', async () => {
    let release!: () => void
    const gate = new Promise<void>((resolve) => { release = resolve })
    mockFetch(async (req) => {
      await gate
      if (req.path.includes('/members')) {
        return ok([{ user_id: 'user-a', role: 'admin' }])
      }
      throw new Error(`unexpected request ${req.path}`)
    })

    const roster = loadDMRoster('workspace-a', 'channel-a')
    const role = loadWorkspaceRole('workspace-a', 'user-a')
    await flush()
    clearDMRosterCache()
    clearWorkspaceRoleCache()
    release()
    await Promise.all([roster, role])

    expect(getCachedDMRoster('channel-a')).toBeUndefined()
    expect(getCachedWorkspaceRole('workspace-a:user-a')).toBeUndefined()
  })
})
