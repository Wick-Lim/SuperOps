import { useEffect, useSyncExternalStore } from 'react'
import { emojiApi, type CustomEmoji } from '../../api/emoji'
import { useWorkspaceStore } from '../../stores/workspaceStore'
import { API_BASE_URL } from '../../config'

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

const cache = new Map<string, CustomEmoji[]>()
const inFlight = new Set<string>()
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
export function loadCustomEmoji(workspaceId: string | undefined): void {
  if (!workspaceId || cache.has(workspaceId) || inFlight.has(workspaceId)) return
  inFlight.add(workspaceId)
  emojiApi
    .list(workspaceId)
    .then((res) => cache.set(workspaceId, res.data ?? EMPTY))
    .catch(() => cache.set(workspaceId, EMPTY))
    .finally(() => {
      inFlight.delete(workspaceId)
      emit()
    })
}

/** Custom emoji for the active workspace; `[]` until they load. */
export function useCustomEmoji(): CustomEmoji[] {
  const workspaceId = useWorkspaceStore((s) => s.activeWorkspace?.id)
  const list = useSyncExternalStore(subscribe, () =>
    workspaceId ? cache.get(workspaceId) ?? EMPTY : EMPTY,
  )
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
