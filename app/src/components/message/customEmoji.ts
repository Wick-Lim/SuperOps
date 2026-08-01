import { useEffect, useSyncExternalStore } from 'react'
import { emojiApi, type CustomEmoji } from '../../api/emoji'
import { useWorkspaceStore } from '../../stores/workspaceStore'
import { API_BASE_URL } from '../../config'
import { AccountCache } from '../../lib/accountCache'

/**
 * Workspace custom emoji, cached per workspace outside React.
 *
 * `api/emoji.ts` had zero importers: the picker was a hardcoded array and a
 * workspace's own emoji were unreachable. This is deliberately not a Zustand
 * slice — `src/stores` is owned elsewhere — but it behaves like one via
 * `useSyncExternalStore`, so the snapshot identity is stable and a row that
 * renders a reaction does not re-render when an unrelated workspace loads.
 */

const EMPTY: CustomEmoji[] = []

const cache = new AccountCache<string, CustomEmoji[]>()
const inFlight = new Map<string, number>()
const listeners = new Set<() => void>()

function emit() {
  listeners.forEach((l) => l())
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener)
  return () => {
    listeners.delete(listener)
  }
}

/**
 * Fetches once per workspace. A failure caches the empty list rather than
 * retrying: custom emoji are decoration, and a workspace the caller cannot read
 * them for would otherwise re-request on every picker open.
 */
export function getCustomEmojiSnapshot(workspaceId?: string): CustomEmoji[] {
  return workspaceId ? cache.get(workspaceId) ?? EMPTY : EMPTY
}

export function clearCustomEmojiCache(): void {
  cache.clear()
  inFlight.clear()
  emit()
}

export function loadCustomEmoji(workspaceId: string | undefined): void {
  if (!workspaceId || cache.has(workspaceId) || inFlight.has(workspaceId)) return
  const generation = cache.generation
  inFlight.set(workspaceId, generation)
  emojiApi
    .list(workspaceId)
    .then((res) => {
      cache.setIfCurrent(generation, workspaceId, res.data ?? EMPTY)
    })
    .catch(() => {
      cache.setIfCurrent(generation, workspaceId, EMPTY)
    })
    .finally(() => {
      if (inFlight.get(workspaceId) === generation) inFlight.delete(workspaceId)
      if (generation === cache.generation) emit()
    })
}

/** Custom emoji for the active workspace; `[]` until they load. */
export function useCustomEmoji(): CustomEmoji[] {
  const workspaceId = useWorkspaceStore((s) => s.activeWorkspace?.id)
  const list = useSyncExternalStore(subscribe, () => getCustomEmojiSnapshot(workspaceId))
  useEffect(() => {
    loadCustomEmoji(workspaceId)
  }, [workspaceId])
  return list
}

/** `:party_parrot:` → `party_parrot`; anything else → null. */
export function customEmojiName(token: string): string | null {
  const m = /^:([a-z0-9_+-]+):$/i.exec(token)
  return m ? m[1] : null
}

export function findCustomEmoji(list: CustomEmoji[], token: string): CustomEmoji | null {
  const name = customEmojiName(token)
  if (!name) return null
  return list.find((e) => e.name === name) ?? null
}

/** `image_url` may be stored relative to the API root. */
export function customEmojiUrl(emoji: CustomEmoji): string {
  const url = emoji.image_url || ''
  if (/^[a-z][a-z0-9+.-]*:/i.test(url)) return url
  return `${API_BASE_URL}${url.startsWith('/') ? '' : '/'}${url}`
}
