# Phase 1 Client Data Safety Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Make logout and terminal authentication failure a complete account-isolation boundary, and make collaborative editing report `saved` only after a local update is durably acknowledged while recovering the send/disconnect race in memory.

**Architecture:** A single `resetAccountSession()` coordinator synchronously invalidates API/WebSocket generations, clears every account-scoped store and generational cache, and only then marks authentication false; remote cleanup runs afterward and cannot restore state. `CollabProvider` owns a memory-only merged pending Yjs update, a connection-aware delivery state machine, and the only contiguous projection watermark; reconnect recovery always performs HTTP catch-up before durable HTTP append and does not return to `synced` until the append sequence is contiguous locally.

**Tech Stack:** TypeScript 5.9, Expo 55, React 19, React Native 0.83, Zustand 5, Yjs 13, Vitest 3, the existing authenticated REST client, and the existing WebSocket collaboration protocol.

## Global Constraints

- Work occurs on an isolated worktree and a `codex/` branch, never directly on `main`.
- Every behavior change follows red-green-refactor TDD; run the named focused test and observe the stated failure before editing production code.
- Do not add a runtime dependency. Use the existing Expo, React Native, Zustand, Yjs, REST, and WebSocket facilities.
- Phase 1 is fail-closed during an outage and does not add a persistent offline document cache.
- Only updates created in the send/disconnect race are retained, in memory, until durable recovery succeeds or the provider is destroyed.
- Drive's deployment-wide registry and non-account UI preferences survive session reset; account-specific navigation and content do not.
- `isAuthenticated` must become `false` only after every other account-scoped selector has been synchronously emptied.
- A WebSocket `send() === true` means only that an open platform socket accepted the frame; only an own-connection echo or HTTP `201 {seq}` proves durability.
- A projection sequence is the provider's highest contiguous locally applied sequence. A descriptor head or an out-of-order sequence must never be published as that watermark.
- Revocation and provider destruction win every race and prevent late async callbacks from changing the document or screen status.
- Existing public behavior remains compatible except for unsafe stale-session requests, which terminate with the new synthetic `SESSION_CHANGED` error.

The approved program summary lists projection freshness under Phase 2, while its approved detailed Phase 1 design and Phase 1 acceptance tests explicitly require provider-owned projection watermarks. This plan follows the more specific detailed contract, so projection watermark ownership is completed in Task 6 before Phase 1 is accepted.

---

## File and Responsibility Map

### New files

- `app/src/lib/accountCache.ts` — small generational cache primitive; a clear invalidates old async writers.
- `app/src/components/channel/dmRosterCache.ts` — account-scoped DM roster loading and caching without importing `ChannelView` into session infrastructure.
- `app/src/lib/accountSession.ts` — the one production session-reset coordinator and terminal-bootstrap error classifier.
- `app/test/session.test.ts` — cross-store/cache/account A → B isolation and remote-cleanup failure coverage.

### Existing files changed

- `app/src/stores/workspaceStore.ts` and `app/src/stores/uiStore.ts` — add complete, idempotent account-data clears.
- `app/src/stores/userStore.ts` — generation-fence old user lookups after clear.
- `app/src/components/channel/ChannelView.tsx` — consume `dmRosterCache` instead of a module-global `Map`.
- `app/src/components/message/customEmoji.ts` — generational async cache and explicit reset/read surface.
- `app/src/screens/internal/useWorkspaceRole.ts` — generational async role cache and explicit reset/read surface.
- `app/src/lib/errors.ts` and `app/src/api/client.ts` — stale-request `SESSION_CHANGED`, API session epoch, refresh identity guard, and `api.resetSession()`.
- `app/src/lib/websocket.ts` — lifecycle epoch, complete account reset, stale callback/resync fences, and boolean send acceptance.
- `app/src/stores/authStore.ts` — state/storage-only auth cleanup plus dynamic delegation to the coordinator.
- `app/src/navigation/AppNavigator.tsx` — terminal bootstrap 401/403 uses the same reset boundary.
- `app/src/lib/collab/provider.ts` — saving state, merged pending update, own-echo acknowledgement, catch-up/append recovery, retry, async guards, and projection watermark callback.
- `app/src/screens/CollabDocumentScreen.tsx` — latest provider watermark, saving/retry presentation, and one editability rule for document/sheet/design.
- `app/test/helpers.ts` — complete deterministic test reset and controllable WebSocket send failure.
- `app/test/stores.test.ts`, `app/test/client.test.ts`, `app/test/websocket.test.ts`, `app/test/collab.test.ts`, `app/test/projection.test.ts`, and `app/test/sheet.test.ts` — behavior regressions for every Phase 1 invariant.

---

### Task 1: Idempotent stores and generational account caches

**Files:**
- Create: `app/src/lib/accountCache.ts`
- Create: `app/src/components/channel/dmRosterCache.ts`
- Modify: `app/src/stores/workspaceStore.ts`
- Modify: `app/src/stores/uiStore.ts`
- Modify: `app/src/stores/userStore.ts`
- Modify: `app/src/components/channel/ChannelView.tsx`
- Modify: `app/src/components/message/customEmoji.ts`
- Modify: `app/src/screens/internal/useWorkspaceRole.ts`
- Modify: `app/test/helpers.ts`
- Test: `app/test/stores.test.ts`
- Test: `app/test/session.test.ts`

**Interfaces:**
- Consumes: existing Zustand `clear()` actions in `useChannelStore`, `useMessageStore`, `useDriveStore`, and `useUserStore`.
- Produces: `AccountCache<K, V>`, `useWorkspaceStore.getState().clear(): void`, `useUiStore.getState().clear(): void`, `clearDMRosterCache(): void`, `clearCustomEmojiCache(): void`, `clearWorkspaceRoleCache(): void`, and generation-fenced async cache loaders for Task 3's coordinator.

- [ ] **Step 1: Add failing store and cache-isolation tests**

Replace the first Vitest import, extend the helper import, add fetch cleanup, and then add these cases to `app/test/stores.test.ts`:

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { useDriveStore } from '../src/stores/driveStore'
import { useUiStore } from '../src/stores/uiStore'
import { useUserStore } from '../src/stores/userStore'
import { useWorkspaceStore } from '../src/stores/workspaceStore'
import { flush, makeChannel, makeMessage, ok, resetStores } from './helpers'

const realFetch = globalThis.fetch
afterEach(() => { globalThis.fetch = realFetch })

it('clears workspace and UI account data idempotently', () => {
  useWorkspaceStore.setState({
    workspaces: [{ id: 'ws-a', name: 'A' } as never],
    activeWorkspace: { id: 'ws-a', name: 'A' } as never,
  })
  useUiStore.setState({
    presence: { 'user-a': 'online' },
    typing: { 'channel-a': ['user-a'] },
    unreadNotifications: 7,
    connection: 'connected',
    connectionError: 'OLD_ERROR',
    activeThread: { channelId: 'channel-a', parent: makeMessage('parent-a', 'channel-a') },
  })

  useWorkspaceStore.getState().clear()
  useUiStore.getState().clear()
  useWorkspaceStore.getState().clear()
  useUiStore.getState().clear()

  expect(useWorkspaceStore.getState().workspaces).toEqual([])
  expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
  expect(useUiStore.getState()).toMatchObject({
    presence: {},
    typing: {},
    unreadNotifications: 0,
    connection: 'idle',
    connectionError: null,
    activeThread: null,
  })
})

it('clears Drive navigation but keeps the deployment registry', () => {
  useDriveStore.setState({
    registry: [{ type: 'document', label: 'Document', creatable: true } as never],
    registryLoaded: true,
    rootId: 'root-a',
    entries: [{ kind: 'file', file: { id: 'file-a' } } as never],
    hasMore: true,
  })

  useDriveStore.getState().clear()

  expect(useDriveStore.getState().registry).toHaveLength(1)
  expect(useDriveStore.getState()).toMatchObject({
    registryLoaded: true,
    rootId: null,
    folder: null,
    breadcrumb: [],
    entries: [],
    cursor: null,
    hasMore: false,
  })
})

it('ignores a user lookup that completes after clear', async () => {
  let resolve!: (value: Response) => void
  globalThis.fetch = vi.fn(() => new Promise<Response>((r) => { resolve = r })) as typeof fetch

  useUserStore.getState().ensureUsers(['user-a'])
  useUserStore.getState().clear()
  resolve(ok({ id: 'user-a', username: 'old-account' } as never))
  await flush(20)

  expect(useUserStore.getState().users).toEqual({})
  expect(useUserStore.getState().pending.size).toBe(0)
  expect(useUserStore.getState().failed.size).toBe(0)
})
```

Create `app/test/session.test.ts` with cache behavior tests. These tests use production reads rather than test-only counters:

```ts
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
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
```

- [ ] **Step 2: Run the focused tests and confirm red**

Run:

```bash
cd app
npx vitest run test/stores.test.ts test/session.test.ts
```

Expected: TypeScript/module collection fails because `AccountCache`, the cache reset/read/load functions, and the workspace/UI `clear()` actions do not exist. If collection reaches runtime, the delayed user lookup assertion also fails because the old promise repopulates the store.

- [ ] **Step 3: Implement the generational cache primitive and pure DM cache**

Create `app/src/lib/accountCache.ts`:

```ts
export class AccountCache<K, V> {
  private values = new Map<K, V>()
  private epoch = 0

  get generation(): number {
    return this.epoch
  }

  get(key: K): V | undefined {
    return this.values.get(key)
  }

  has(key: K): boolean {
    return this.values.has(key)
  }

  set(key: K, value: V): void {
    this.values.set(key, value)
  }

  setIfCurrent(generation: number, key: K, value: V): boolean {
    if (generation !== this.epoch) return false
    this.values.set(key, value)
    return true
  }

  clear(): void {
    this.epoch += 1
    this.values.clear()
  }
}
```

Create `app/src/components/channel/dmRosterCache.ts`:

```ts
import { channelApi } from '../../api/channels'
import { AccountCache } from '../../lib/accountCache'

const rosters = new AccountCache<string, readonly string[]>()

export function getCachedDMRoster(channelId: string): readonly string[] | undefined {
  return rosters.get(channelId)
}

export async function loadDMRoster(
  workspaceId: string,
  channelId: string,
): Promise<readonly string[] | undefined> {
  const cached = rosters.get(channelId)
  if (cached) return cached
  const generation = rosters.generation
  const res = await channelApi.listMembers(workspaceId, channelId)
  const ids = (res.data ?? []).map((member) => member.user_id)
  return rosters.setIfCurrent(generation, channelId, ids) ? ids : undefined
}

export function clearDMRosterCache(): void {
  rosters.clear()
}
```

Replace `ChannelView.tsx`'s module-global `dmMembers` reads/writes with `getCachedDMRoster()` and `loadDMRoster()`. The effect must ignore `undefined`, because that value means a session reset invalidated the request:

```ts
const [dmIds, setDmIds] = useState<string[]>(() => [
  ...(getCachedDMRoster(channel.id) ?? []),
])

useEffect(() => {
  if (!isDM || channel.name) return
  const cached = getCachedDMRoster(channel.id)
  if (cached) {
    const ids = [...cached]
    setDmIds(ids)
    ensureUsers(ids)
    return
  }
  let cancelled = false
  void loadDMRoster(channel.workspace_id, channel.id)
    .then((loaded) => {
      if (cancelled || !loaded) return
      const ids = [...loaded]
      setDmIds(ids)
      ensureUsers(ids)
    })
    .catch(() => undefined)
  return () => { cancelled = true }
}, [channel.id, channel.name, channel.workspace_id, ensureUsers, isDM])
```

- [ ] **Step 4: Add store clears and fence old user requests**

Extend `WorkspaceState` and its initial state:

```ts
interface WorkspaceState {
  workspaces: Workspace[]
  activeWorkspace: Workspace | null
  setWorkspaces: (ws: Workspace[]) => void
  setActiveWorkspace: (ws: Workspace) => void
  clear: () => void
}

clear: () => set({ workspaces: [], activeWorkspace: null }),
```

Extend `UiState` and its initial state:

```ts
clear: () => void

clear: () => set({
  presence: {},
  typing: {},
  unreadNotifications: 0,
  connection: 'idle',
  connectionError: null,
  activeThread: null,
}),
```

In `userStore.ts`, add a request generation next to `attempts`, capture it in `ensureUsers`, guard both completion branches, and increment it in `clear()`:

```ts
const attempts = new Map<string, number>()
let generation = 0

ensureUsers: (ids) => {
  const requestGeneration = generation
  const { users, pending, failed } = get()
  const missing = Array.from(
    new Set(ids.filter((id) => id && !users[id] && !pending.has(id) && !failed.has(id))),
  )
  if (missing.length === 0) return
  set({ pending: withAdded(pending, missing) })
  missing.forEach((id) => {
    userApi.get(id).then((res) => {
      if (requestGeneration !== generation) return
      attempts.delete(id)
      set((state) => ({
        users: { ...state.users, [id]: res.data },
        pending: withRemoved(state.pending, id),
      }))
    }).catch((error: unknown) => {
      if (requestGeneration !== generation) return
      const permanent = isApiError(error) && error.status >= 400 && error.status < 500
      const tries = (attempts.get(id) ?? 0) + 1
      attempts.set(id, tries)
      set((state) => ({
        pending: withRemoved(state.pending, id),
        failed: permanent || tries >= MAX_ATTEMPTS ? withAdded(state.failed, [id]) : state.failed,
      }))
    })
  })
},

clear: () => {
  generation += 1
  attempts.clear()
  set({ users: {}, pending: new Set(), failed: new Set() })
},
```

- [ ] **Step 5: Make custom emoji and workspace role caches generational**

In `customEmoji.ts`, replace the bare `Map`/`Set` with `AccountCache` plus a generation-tagged in-flight map. Export a stable empty snapshot, a reset, and the existing loader:

```ts
const cache = new AccountCache<string, CustomEmoji[]>()
const inFlight = new Map<string, number>()
const listeners = new Set<() => void>()

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
  emojiApi.list(workspaceId).then((res) => {
    cache.setIfCurrent(generation, workspaceId, res.data ?? EMPTY)
  }).catch(() => {
    cache.setIfCurrent(generation, workspaceId, EMPTY)
  }).finally(() => {
    if (inFlight.get(workspaceId) === generation) inFlight.delete(workspaceId)
    if (generation === cache.generation) emit()
  })
}
```

Make `useCustomEmoji()` call `getCustomEmojiSnapshot(workspaceId)` in its `useSyncExternalStore` snapshot.

In `useWorkspaceRole.ts`, replace the bare map with an `AccountCache` and move the async operation into an exported loader so it can be behavior-tested:

```ts
const cache = new AccountCache<string, WorkspaceRole | null>()

export function getCachedWorkspaceRole(key: string): WorkspaceRole | null | undefined {
  return cache.get(key)
}

export function clearWorkspaceRoleCache(): void {
  cache.clear()
}

export async function loadWorkspaceRole(
  workspaceId: string,
  userId: string,
): Promise<WorkspaceRole | null | undefined> {
  const key = `${workspaceId}:${userId}`
  if (cache.has(key)) return cache.get(key)
  const generation = cache.generation
  const res = await workspaceApi.listMembers(workspaceId)
  const role = (res.data ?? []).find((member) => member.user_id === userId)?.role ?? null
  return cache.setIfCurrent(generation, key, role) ? role : undefined
}
```

The hook effect calls `loadWorkspaceRole()` and only calls `setRole()` when it is neither cancelled nor `undefined`; its catch path still returns the safe non-admin result `null`.

- [ ] **Step 6: Complete the shared test reset, run green, and commit**

Update `resetStores()` in `app/test/helpers.ts` to clear Drive, users, the active thread, and all three account caches. Keep it synchronous; Task 2 will add API/WebSocket epoch reset here.

Run:

```bash
cd app
npx vitest run test/stores.test.ts test/session.test.ts
npm run typecheck
```

Expected: both focused files pass and TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/accountCache.ts app/src/components/channel/dmRosterCache.ts app/src/components/channel/ChannelView.tsx app/src/components/message/customEmoji.ts app/src/screens/internal/useWorkspaceRole.ts app/src/stores/workspaceStore.ts app/src/stores/uiStore.ts app/src/stores/userStore.ts app/test/helpers.ts app/test/stores.test.ts app/test/session.test.ts
git commit -m "fix: clear account-scoped client caches"
```

---

### Task 2: Fence stale API and WebSocket work across accounts

**Files:**
- Modify: `app/src/lib/errors.ts`
- Modify: `app/src/api/client.ts`
- Modify: `app/src/lib/websocket.ts`
- Modify: `app/test/helpers.ts`
- Test: `app/test/client.test.ts`
- Test: `app/test/websocket.test.ts`

**Interfaces:**
- Consumes: Task 1's complete store `clear()` actions and generational caches.
- Produces: `ApiErrorCode.SessionChanged`, `api.resetSession(): void`, and a `wsManager.reset(): void` that invalidates every old socket callback, delayed resync, listener, subscription, timer, connection identifier, and cooldown.

- [ ] **Step 1: Add failing API epoch tests**

Add deterministic deferred responses to `app/test/client.test.ts`:

```ts
it('rejects an A response that arrives after reset and preserves B', async () => {
  let release!: () => void
  net = mockFetch(async () => {
    await new Promise<void>((resolve) => { release = resolve })
    return ok({ owner: 'account-a' })
  })
  signIn('access-a', 'refresh-a', 'user-a')
  const oldRequest = api.get<{ owner: string }>('/slow')
  await Promise.resolve()

  api.resetSession()
  signIn('access-b', 'refresh-b', 'user-b')
  release()
  const error = await oldRequest.then(() => null).catch((value: unknown) => value)

  expect((error as ApiError).code).toBe(ApiErrorCode.SessionChanged)
  expect(useAuthStore.getState()).toMatchObject({
    accessToken: 'access-b',
    refreshToken: 'refresh-b',
    user: { id: 'user-b' },
  })
})

it('does not let an A refresh restore tokens after reset', async () => {
  let releaseRefresh!: () => void
  net = mockFetch(async (req) => {
    if (req.path === '/auth/refresh') {
      await new Promise<void>((resolve) => { releaseRefresh = resolve })
      return ok({ access_token: 'revived-a', refresh_token: 'revived-refresh-a' })
    }
    return apiFailure(401, ServerErrorCode.Unauthorized)
  })
  signIn('access-a', 'refresh-a', 'user-a')
  const oldRequest = api.get('/needs-refresh').catch((error: unknown) => error)
  await flush(10)

  api.resetSession()
  signIn('access-b', 'refresh-b', 'user-b')
  releaseRefresh()
  await oldRequest

  expect(useAuthStore.getState()).toMatchObject({
    accessToken: 'access-b',
    refreshToken: 'refresh-b',
    user: { id: 'user-b' },
  })
})
```

Add this third case in the same describe block. It proves both epoch separation and the promise-identity cleanup guard:

```ts
it('does not let A refresh completion detach B callers from B single-flight', async () => {
  let releaseA!: () => void
  let releaseB!: () => void
  const gateA = new Promise<void>((resolve) => { releaseA = resolve })
  const gateB = new Promise<void>((resolve) => { releaseB = resolve })
  const refreshTokens: string[] = []
  net = mockFetch(async (req) => {
    if (req.path === '/auth/refresh') {
      const token = (req.body as { refresh_token: string }).refresh_token
      refreshTokens.push(token)
      if (token === 'refresh-a') {
        await gateA
        return ok({ access_token: 'fresh-a', refresh_token: 'rotated-a' })
      }
      await gateB
      return ok({ access_token: 'fresh-b', refresh_token: 'rotated-b' })
    }
    if (req.headers.Authorization === 'Bearer fresh-b') return ok({ path: req.path })
    return apiFailure(401, ServerErrorCode.Unauthorized)
  })

  signIn('access-a', 'refresh-a', 'user-a')
  const oldA = api.get('/a').catch((error: unknown) => error)
  await flush(10)
  api.resetSession()
  signIn('access-b', 'refresh-b', 'user-b')
  const b1 = api.get('/b-1')
  const b2 = api.get('/b-2')
  await flush(10)

  expect(refreshTokens).toEqual(['refresh-a', 'refresh-b'])
  releaseA()
  await oldA
  expect(refreshTokens).toEqual(['refresh-a', 'refresh-b'])
  releaseB()
  await expect(Promise.all([b1, b2])).resolves.toHaveLength(2)
  expect(refreshTokens.filter((token) => token === 'refresh-b')).toHaveLength(1)
})
```

- [ ] **Step 2: Run the API test and confirm red**

Run:

```bash
cd app
npx vitest run test/client.test.ts
```

Expected: collection fails because `SessionChanged` and `api.resetSession()` do not exist; without the epoch guard the delayed A refresh overwrites B's tokens.

- [ ] **Step 3: Implement the API session epoch and refresh identity guard**

Add this synthetic code to `ApiErrorCode` in `errors.ts`:

```ts
/** A request belongs to an account session that has already ended. */
SessionChanged: 'SESSION_CHANGED',
```

In `ApiClient`, add:

```ts
private sessionEpoch = 0
private refreshInFlight: Promise<boolean> | null = null

resetSession(): void {
  this.sessionEpoch += 1
  this.refreshInFlight = null
}

private assertCurrentSession(epoch: number): void {
  if (epoch !== this.sessionEpoch) {
    throw new ApiError(0, ApiErrorCode.SessionChanged, 'This request belonged to a previous sign-in.')
  }
}
```

At the beginning of `request()` and `upload()`, capture `const epoch = this.sessionEpoch`. Call `assertCurrentSession(epoch)` after every network/body-read/refresh await and before retrying or returning a response. Change refresh to receive its epoch and original token:

```ts
private tryRefresh(epoch: number): Promise<boolean> {
  this.assertCurrentSession(epoch)
  if (this.refreshInFlight) return this.refreshInFlight
  const refreshToken = useAuthStore.getState().refreshToken
  if (!refreshToken) return Promise.resolve(false)
  const inFlight = this.doRefresh(epoch, refreshToken).then(
    (ok) => {
      if (this.refreshInFlight === inFlight) this.refreshInFlight = null
      return ok
    },
    () => {
      if (this.refreshInFlight === inFlight) this.refreshInFlight = null
      return false
    },
  )
  this.refreshInFlight = inFlight
  return inFlight
}

private async doRefresh(epoch: number, refreshToken: string): Promise<boolean> {
  try {
    const res = await fetchWithTimeout(
      `${API_BASE_URL}/auth/refresh`,
      {
        method: 'POST',
        headers: { 'Content-Type': 'application/json' },
        body: JSON.stringify({ refresh_token: refreshToken }),
      },
      REQUEST_TIMEOUT_MS,
    )
    this.assertCurrentSession(epoch)
    if (!res.ok) return false
    const envelope = await readEnvelope(res)
    this.assertCurrentSession(epoch)
    const pair = envelope?.data as { access_token?: string; refresh_token?: string } | undefined
    if (!pair?.access_token || !pair.refresh_token) return false
    if (useAuthStore.getState().refreshToken !== refreshToken) return false
    await useAuthStore.getState().setTokens(pair.access_token, pair.refresh_token)
    this.assertCurrentSession(epoch)
    return true
  } catch (error) {
    if (error instanceof ApiError && error.code === ApiErrorCode.SessionChanged) throw error
    return false
  }
}
```

Update both `request()` and `upload()` to call `tryRefresh(epoch)`. Do not turn `SESSION_CHANGED` into `SESSION_EXPIRED`, because that would let an old A request sign out B.

- [ ] **Step 4: Add failing WebSocket lifecycle tests**

In `app/test/websocket.test.ts`, add behavior cases using the existing fake socket and mock fetch:

```ts
it('ignores callbacks from a socket that existed before reset', async () => {
  const oldSocket = connectOpen()
  let roomEvents = 0
  let statusEvents = 0
  wsManager.onRoom(() => { roomEvents += 1 })
  wsManager.onStatus(() => { statusEvents += 1 })
  wsManager.subscribe('channel-a')
  wsManager.joinRoom('document-a')

  wsManager.reset()
  const statusAtReset = statusEvents
  oldSocket.emit({ type: 'hello', seq: 1, data: { connection_id: 'old-connection' } })
  oldSocket.emit({
    type: 'collab.joined',
    seq: 2,
    data: { document_id: 'document-a', head_seq: 9, can_write: true },
  })
  await flush()

  expect(roomEvents).toBe(0)
  expect(statusEvents).toBe(statusAtReset)
  expect(wsManager.getConnectionId()).toBeNull()
  expect(useUiStore.getState().connection).toBe('idle')
})

it('does not apply an A resync response after reset or carry its cooldown to B', async () => {
  let releaseA!: () => void
  let requestCount = 0
  mockFetch(async (req) => {
    requestCount += 1
    if (requestCount === 1) {
      await new Promise<void>((resolve) => { releaseA = resolve })
      return ok([makeChannel('channel-a')])
    }
    if (req.path.includes('/channels')) return ok([makeChannel('channel-b')])
    if (req.path.includes('/unread-count')) return ok({ count: 0 })
    return ok([])
  })
  useWorkspaceStore.setState({ activeWorkspace: { id: 'workspace-a' } as never })
  const stale = wsManager.resync('manual')
  await flush()

  wsManager.reset()
  useWorkspaceStore.setState({ activeWorkspace: { id: 'workspace-b' } as never })
  await wsManager.resync('manual')
  releaseA()
  await stale

  expect(useChannelStore.getState().channels.map((channel) => channel.id)).toEqual(['channel-b'])
  expect(requestCount).toBeGreaterThan(1)
})
```

Also add this subscription-isolation case:

```ts
it('does not replay A subscriptions or rooms on B connection', () => {
  signIn('access-a', 'refresh-a', 'user-a')
  const accountA = connectOpen()
  wsManager.subscribe('channel-a')
  wsManager.joinRoom('document-a')
  expect(accountA.sentOfType('subscribe')).toHaveLength(1)
  expect(accountA.sentOfType('collab.join')).toHaveLength(1)

  wsManager.reset()
  signIn('access-b', 'refresh-b', 'user-b')
  wsManager.connect()
  const accountB = FakeWebSocket.last
  accountB.openNow()

  expect(accountB.sentOfType('subscribe')).toHaveLength(0)
  expect(accountB.sentOfType('collab.join')).toHaveLength(0)
})
```

- [ ] **Step 5: Run the WebSocket test and confirm red**

Run:

```bash
cd app
npx vitest run test/websocket.test.ts
```

Expected: the old status listener is called, the old socket frame can mutate connection state, the delayed A resync can overwrite B, or B's immediate resync is suppressed by A's cooldown.

- [ ] **Step 6: Implement complete WebSocket reset and async generation fences**

Add `private lifecycleGeneration = 0` to `WebSocketManager`. In `connect()`, capture `const generation = this.lifecycleGeneration` and make each socket callback begin with:

```ts
if (generation !== this.lifecycleGeneration || this.ws !== socket) return
```

Before closing a socket during full `reset()`, detach its four callbacks so a queued platform callback cannot enter old code:

```ts
const socket = this.ws
if (socket) {
  socket.onopen = null
  socket.onmessage = null
  socket.onerror = null
  socket.onclose = null
}
```

At the start of `resync()`, capture the generation. Check it after every await and before every store write/listener notification. Its `finally` may clear `resyncInFlight` only if the generation is still current. Make `reset()` perform all of these operations synchronously:

```ts
reset() {
  this.lifecycleGeneration += 1
  this.handlers.clear()
  this.statusListeners.clear()
  this.resyncListeners.clear()
  this.roomListeners.clear()
  this.huddleListeners.clear()
  this.resyncInFlight = false
  this.lastResyncAt = 0
  this.disconnect()
  this.everConnected = false
  this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
}
```

Retain `disconnect()` as the non-account teardown used by normal lifecycle code; only `reset()` removes listeners and invalidates async work.

- [ ] **Step 7: Add API/WebSocket reset to the shared helper, run green, and commit**

Make `resetStores()` call `api.resetSession()` and `wsManager.reset()` before manually restoring auth test hydration and clearing the Task 1 stores/caches. Then run:

```bash
cd app
npx vitest run test/client.test.ts test/websocket.test.ts
npm run typecheck
```

Expected: both focused test files pass, the existing same-session refresh coalescing test remains green, and TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/errors.ts app/src/api/client.ts app/src/lib/websocket.ts app/test/helpers.ts app/test/client.test.ts app/test/websocket.test.ts
git commit -m "fix: fence stale client work across sessions"
```

---

### Task 3: Centralize local-first logout and terminal bootstrap reset

**Files:**
- Create: `app/src/lib/accountSession.ts`
- Modify: `app/src/stores/authStore.ts`
- Modify: `app/src/navigation/AppNavigator.tsx`
- Modify: `app/test/helpers.ts`
- Test: `app/test/session.test.ts`
- Test: `app/test/client.test.ts`

**Interfaces:**
- Consumes: `api.resetSession(): void`, the strengthened `wsManager.reset(): void`, Task 1 store/cache clears, `deregisterPushToken(accessToken): Promise<void>`, and `authApi.logout(refreshToken)`.
- Produces: `resetAccountSession(): Promise<void>`, `isTerminalBootstrapAuthError(error: unknown): boolean`, `clearLocalAuthSession(): Promise<void>`, and an unchanged `useAuthStore.getState().logout(): Promise<void>` public call.

- [ ] **Step 1: Add failing full-session and account A → B tests**

Extend `app/test/session.test.ts` with a fixture that fills every account selector and cache, then test the production coordinator. Replace Task 1's synchronous `beforeEach` and simple `afterEach` with the async-storage-aware hooks shown here:

```ts
import AsyncStorage from '@react-native-async-storage/async-storage'
import { resetAccountSession, isTerminalBootstrapAuthError } from '../src/lib/accountSession'
import { ApiError } from '../src/lib/errors'
import { useAuthStore } from '../src/stores/authStore'
import { useChannelStore } from '../src/stores/channelStore'
import { useDriveStore } from '../src/stores/driveStore'
import { useMessageStore } from '../src/stores/messageStore'
import { useUiStore } from '../src/stores/uiStore'
import { useUserStore } from '../src/stores/userStore'
import { useWorkspaceStore } from '../src/stores/workspaceStore'
import { makeChannel, makeMessage, signIn } from './helpers'

beforeEach(async () => {
  resetStores()
  await AsyncStorage.clear()
})

afterEach(() => {
  globalThis.fetch = realFetch
  vi.restoreAllMocks()
})

function populateAccountA(): void {
  signIn('access-a', 'refresh-a', 'user-a')
  useAuthStore.setState({ hydrated: true })
  useWorkspaceStore.setState({
    workspaces: [{ id: 'workspace-a' } as never],
    activeWorkspace: { id: 'workspace-a' } as never,
  })
  useChannelStore.setState({
    channels: [makeChannel('channel-a')],
    activeChannel: makeChannel('channel-a'),
  })
  useMessageStore.setState({
    messages: { 'channel-a': [makeMessage('message-a', 'channel-a')] },
    cursors: { 'channel-a': 'cursor-a' },
    hasMore: { 'channel-a': true },
  })
  useDriveStore.setState({
    registry: [{ type: 'document', creatable: true } as never],
    registryLoaded: true,
    rootId: 'root-a',
    entries: [{ kind: 'file', file: { id: 'file-a' } } as never],
  })
  useUserStore.setState({
    users: { 'user-a': { id: 'user-a', username: 'account-a' } as never },
  })
  useUiStore.setState({
    presence: { 'user-a': 'online' },
    typing: { 'channel-a': ['user-a'] },
    unreadNotifications: 4,
    connection: 'connected',
    activeThread: { channelId: 'channel-a', parent: makeMessage('parent-a', 'channel-a') },
  })
}

it('empties all account state before remote cleanup settles', async () => {
  populateAccountA()
  await AsyncStorage.setItem('superops-push-token', 'push-a')
  let releaseRemote!: () => void
  const gate = new Promise<void>((resolve) => { releaseRemote = resolve })
  mockFetch(async (req) => {
    if (req.path === '/users/me/devices/push-a' || req.path === '/auth/logout') {
      await gate
      return ok({ message: 'ok' })
    }
    throw new Error(`unexpected request ${req.path}`)
  })

  const resetting = resetAccountSession()
  await flush(20)

  expect(useAuthStore.getState()).toMatchObject({
    accessToken: null,
    refreshToken: null,
    user: null,
    isAuthenticated: false,
    hydrated: true,
  })
  expect(useWorkspaceStore.getState()).toMatchObject({ workspaces: [], activeWorkspace: null })
  expect(useChannelStore.getState()).toMatchObject({ channels: [], activeChannel: null })
  expect(useMessageStore.getState().messages).toEqual({})
  expect(useDriveStore.getState()).toMatchObject({ rootId: null, entries: [] })
  expect(useDriveStore.getState().registry).toHaveLength(1)
  expect(useUserStore.getState().users).toEqual({})
  expect(useUiStore.getState()).toMatchObject({
    presence: {},
    typing: {},
    unreadNotifications: 0,
    connection: 'idle',
    activeThread: null,
  })

  releaseRemote()
  await expect(resetting).resolves.toBeUndefined()
})

it('starts B with no selectable state from A and is safe twice', async () => {
  populateAccountA()
  mockFetch(() => ok({ message: 'ok' }))
  await resetAccountSession()
  await resetAccountSession()
  signIn('access-b', 'refresh-b', 'user-b')

  expect(useAuthStore.getState().user?.id).toBe('user-b')
  expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
  expect(useChannelStore.getState().activeChannel).toBeNull()
  expect(useMessageStore.getState().messages['channel-a']).toBeUndefined()
  expect(useUiStore.getState().activeThread).toBeNull()
  expect(useDriveStore.getState().rootId).toBeNull()
  expect(useUserStore.getState().users['user-a']).toBeUndefined()
})

it('clears every loaded module cache through the coordinator', async () => {
  signIn('access-a', 'refresh-a', 'user-a')
  mockFetch((req) => {
    if (req.path.includes('/emojis')) {
      return ok([{ id: 'emoji-a', name: 'old', image_url: '/old.png' }])
    }
    if (req.path.includes('/channels/channel-a/members')) {
      return ok([{ user_id: 'user-a' }])
    }
    if (req.path.includes('/workspaces/workspace-a/members')) {
      return ok([{ user_id: 'user-a', role: 'admin' }])
    }
    if (req.path === '/auth/logout') return ok({ message: 'ok' })
    throw new Error(`unexpected request ${req.path}`)
  })

  loadCustomEmoji('workspace-a')
  await Promise.all([
    loadDMRoster('workspace-a', 'channel-a'),
    loadWorkspaceRole('workspace-a', 'user-a'),
  ])
  await flush(20)
  expect(getCustomEmojiSnapshot('workspace-a')).toHaveLength(1)
  expect(getCachedDMRoster('channel-a')).toEqual(['user-a'])
  expect(getCachedWorkspaceRole('workspace-a:user-a')).toBe('admin')

  await resetAccountSession()

  expect(getCustomEmojiSnapshot('workspace-a')).toEqual([])
  expect(getCachedDMRoster('channel-a')).toBeUndefined()
  expect(getCachedWorkspaceRole('workspace-a:user-a')).toBeUndefined()
})

it('classifies only terminal bootstrap authorization failures', () => {
  expect(isTerminalBootstrapAuthError(new ApiError(401, 'UNAUTHORIZED', 'expired'))).toBe(true)
  expect(isTerminalBootstrapAuthError(new ApiError(403, 'FORBIDDEN', 'disabled'))).toBe(true)
  expect(isTerminalBootstrapAuthError(new ApiError(0, 'NETWORK_ERROR', 'offline'))).toBe(false)
  expect(isTerminalBootstrapAuthError(new ApiError(503, 'SERVICE_UNAVAILABLE', 'down'))).toBe(false)
})
```

Add this remote-failure case:

```ts
it('keeps the local boundary when storage and remote cleanup fail', async () => {
  populateAccountA()
  await AsyncStorage.setItem('superops-push-token', 'push-a')
  const remove = vi.spyOn(AsyncStorage, 'removeItem').mockRejectedValue(new Error('storage failed'))
  mockFetch(async () => { throw new Error('network failed') })

  await expect(resetAccountSession()).resolves.toBeUndefined()

  expect(useAuthStore.getState().isAuthenticated).toBe(false)
  expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
  expect(useChannelStore.getState().activeChannel).toBeNull()
  expect(useMessageStore.getState().messages).toEqual({})
  expect(useDriveStore.getState().rootId).toBeNull()
  expect(useUiStore.getState().activeThread).toBeNull()
  expect(useUserStore.getState().users).toEqual({})
  remove.mockRestore()
})
```

- [ ] **Step 2: Run the session test and confirm red**

Run:

```bash
cd app
npx vitest run test/session.test.ts
```

Expected: module collection fails because `accountSession.ts` and `clearLocalAuthSession()` do not exist. The old `authStore.logout()` also leaves account A's stores populated.

- [ ] **Step 3: Separate synchronous auth state clearing from remote logout**

Remove the static `authApi` import from `authStore.ts`. Export a storage-cleanup function that changes Zustand state before creating any awaited boundary:

```ts
let authSessionGeneration = 0
let persistenceTail: Promise<void> = Promise.resolve()

function enqueuePersistence<T>(work: () => Promise<T>): Promise<T> {
  const run = persistenceTail.then(work, work)
  persistenceTail = run.then(() => undefined, () => undefined)
  return run
}

export function clearLocalAuthSession(): Promise<void> {
  authSessionGeneration += 1
  useAuthStore.setState({
    accessToken: null,
    refreshToken: null,
    user: null,
    isAuthenticated: false,
    persistError: null,
  })
  return enqueuePersistence(async () => {
    await Promise.all([
      deleteSecureItem(ACCESS_KEY),
      deleteSecureItem(REFRESH_KEY),
      AsyncStorage.removeItem(USER_KEY).catch(() => undefined),
      AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => undefined),
    ])
  })
}
```

Route `setTokens()` and `setUser()` persistence through `enqueuePersistence()` with these bodies:

```ts
setTokens: async (access, refresh) => {
  const generation = authSessionGeneration
  set({ accessToken: access, refreshToken: refresh, isAuthenticated: true })
  try {
    await enqueuePersistence(async () => {
      if (generation !== authSessionGeneration) return
      await setSecureItem(ACCESS_KEY, access)
      await setSecureItem(REFRESH_KEY, refresh)
    })
    if (generation === authSessionGeneration && get().persistError) {
      set({ persistError: null })
    }
  } catch (error) {
    if (generation === authSessionGeneration) {
      set({ persistError: error instanceof Error ? error.message : 'Could not save your session securely.' })
    }
  }
},

setUser: async (user) => {
  const generation = authSessionGeneration
  set({ user })
  try {
    await enqueuePersistence(async () => {
      if (generation !== authSessionGeneration) return
      await AsyncStorage.setItem(USER_KEY, JSON.stringify(user))
    })
  } catch {
    // The profile is a cache; the authenticated profile endpoint refills it.
  }
},
```

Extract the existing hydrate storage logic into this queued reader and generation-fence its final Zustand write:

```ts
interface StoredAuthSession {
  access: string | null
  refresh: string | null
  user: User | null
}

async function readStoredAuthSession(): Promise<StoredAuthSession> {
  let access = await getSecureItem(ACCESS_KEY)
  let refresh = await getSecureItem(REFRESH_KEY)
  if (!access) {
    const legacy = await AsyncStorage.getItem(LEGACY_AUTH_KEY)
    if (legacy) {
      try {
        const parsed = JSON.parse(legacy) as { accessToken?: string; refreshToken?: string }
        if (parsed.accessToken) {
          access = parsed.accessToken
          refresh = parsed.refreshToken ?? null
          await setSecureItem(ACCESS_KEY, access)
          if (refresh) await setSecureItem(REFRESH_KEY, refresh)
        }
      } catch {
        access = null
        refresh = null
      }
      await AsyncStorage.removeItem(LEGACY_AUTH_KEY).catch(() => undefined)
    }
  }
  let user: User | null = null
  const serialized = await AsyncStorage.getItem(USER_KEY)
  if (serialized) {
    try {
      user = JSON.parse(serialized) as User
    } catch {
      user = null
    }
  }
  return { access, refresh, user }
}

hydrate: async () => {
  const generation = authSessionGeneration
  try {
    const stored = await enqueuePersistence(readStoredAuthSession)
    if (generation !== authSessionGeneration) return
    if (stored.access) {
      set({
        accessToken: stored.access,
        refreshToken: stored.refresh,
        user: stored.user,
        isAuthenticated: true,
        hydrated: true,
      })
    } else {
      set({ hydrated: true })
    }
  } catch {
    if (generation === authSessionGeneration) set({ hydrated: true })
  }
},
```

Serializing these jobs is essential: an A token write already inside native storage may finish after reset begins, so the queued reset deletion must run after that write and before any B write.

Keep `hydrated` unchanged. Replace the `logout` action body with dynamic delegation, which avoids adding `authStore -> accountSession -> wsManager -> authStore` to module initialization:

```ts
logout: async () => {
  const { resetAccountSession } = await import('../lib/accountSession')
  await resetAccountSession()
},
```

- [ ] **Step 4: Implement the single session-reset coordinator**

Create `app/src/lib/accountSession.ts`:

```ts
import { authApi } from '../api/auth'
import { api } from '../api/client'
import { clearDMRosterCache } from '../components/channel/dmRosterCache'
import { clearCustomEmojiCache } from '../components/message/customEmoji'
import { clearWorkspaceRoleCache } from '../screens/internal/useWorkspaceRole'
import { useAuthStore, clearLocalAuthSession } from '../stores/authStore'
import { useChannelStore } from '../stores/channelStore'
import { useDriveStore } from '../stores/driveStore'
import { useMessageStore } from '../stores/messageStore'
import { useUiStore } from '../stores/uiStore'
import { useUserStore } from '../stores/userStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { isApiError } from './errors'
import { wsManager } from './websocket'

export function isTerminalBootstrapAuthError(error: unknown): boolean {
  return isApiError(error) && (error.status === 401 || error.status === 403)
}

export async function resetAccountSession(): Promise<void> {
  const { accessToken, refreshToken } = useAuthStore.getState()

  api.resetSession()
  wsManager.reset()
  useWorkspaceStore.getState().clear()
  useChannelStore.getState().clear()
  useMessageStore.getState().clear()
  useDriveStore.getState().clear()
  useUserStore.getState().clear()
  useUiStore.getState().clear()
  clearDMRosterCache()
  clearCustomEmojiCache()
  clearWorkspaceRoleCache()

  // This synchronous set is deliberately last: once observers see false,
  // every other account selector above is already empty.
  const localStorageCleanup = clearLocalAuthSession()

  const pushCleanup = import('./push').then(({ deregisterPushToken }) =>
    deregisterPushToken(accessToken),
  )
  const serverCleanup = refreshToken
    ? authApi.logout(refreshToken).then(() => undefined)
    : Promise.resolve()

  await Promise.allSettled([localStorageCleanup, pushCleanup, serverCleanup])
}
```

The coordinator must not catch and rethrow any cleanup error. `Promise.allSettled` is the explicit guarantee that remote/storage failure cannot reverse the local boundary.

- [ ] **Step 5: Route terminal bootstrap 401/403 through the same boundary**

Import `isTerminalBootstrapAuthError` in `AppNavigator.tsx`. Make the workspace bootstrap catch callback async and use this exact ordering:

```ts
.catch(async (error: unknown) => {
  if (cancelled) return
  if (isTerminalBootstrapAuthError(error)) {
    await useAuthStore.getState().logout()
    return
  }
  if (cancelled) return
  setBoot({ status: 'error', message: errorMessage(error, 'Could not reach the server.') })
})
```

This intentionally signs out on both 401 and 403 from the bootstrap endpoint while leaving network and 5xx failures on the retryable bootstrap screen.

- [ ] **Step 6: Prove terminal refresh failure uses the coordinator**

Extend the existing `logs out with SESSION_EXPIRED when the refresh itself fails` case in `app/test/client.test.ts` with this setup before its request and these assertions after its existing error checks. This exercises the existing `api` call to `logout()` and proves the terminal path reaches the coordinator:

```ts
useWorkspaceStore.setState({
  workspaces: [{ id: 'workspace-a' } as never],
  activeWorkspace: { id: 'workspace-a' } as never,
})
useChannelStore.setState({
  channels: [makeChannel('channel-a')],
  activeChannel: makeChannel('channel-a'),
})
useMessageStore.setState({
  messages: { 'channel-a': [makeMessage('message-a', 'channel-a')] },
  cursors: { 'channel-a': 'cursor-a' },
  hasMore: { 'channel-a': true },
})
useDriveStore.setState({ rootId: 'root-a', entries: [{ kind: 'file' } as never] })
useUserStore.setState({ users: { 'user-a': { id: 'user-a' } as never } })
useUiStore.setState({
  presence: { 'user-a': 'online' },
  activeThread: { channelId: 'channel-a', parent: makeMessage('parent-a', 'channel-a') },
})

expect(useWorkspaceStore.getState().activeWorkspace).toBeNull()
expect(useChannelStore.getState().activeChannel).toBeNull()
expect(useMessageStore.getState().messages).toEqual({})
expect(useDriveStore.getState().rootId).toBeNull()
expect(useUserStore.getState().users).toEqual({})
expect(useUiStore.getState()).toMatchObject({ presence: {}, activeThread: null })
```

- [ ] **Step 7: Run focused tests, typecheck, and commit**

Run:

```bash
cd app
npx vitest run test/session.test.ts test/client.test.ts
npm run typecheck
```

Expected: both files pass; the remote gate test observes empty local state before remote completion; terminal refresh failure clears the populated account data; TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/accountSession.ts app/src/stores/authStore.ts app/src/navigation/AppNavigator.tsx app/test/helpers.ts app/test/session.test.ts app/test/client.test.ts
git commit -m "fix: reset the complete account session on logout"
```

---

### Task 4: Make WebSocket send acceptance observable

**Files:**
- Modify: `app/src/lib/websocket.ts`
- Modify: `app/test/helpers.ts`
- Test: `app/test/websocket.test.ts`

**Interfaces:**
- Consumes: Task 2's lifecycle-safe WebSocket manager.
- Produces: `wsManager.send(type: string, data: unknown): boolean` and `wsManager.sendCollabUpdate(documentId: string, updateBase64: string): boolean`. `true` is socket acceptance, not durable acknowledgement.

- [ ] **Step 1: Add failing send-acceptance tests**

Add a one-shot failure flag to `FakeWebSocket` in `app/test/helpers.ts`:

```ts
failNextSend = false

send(raw: string): void {
  if (this.failNextSend) {
    this.failNextSend = false
    throw new Error('platform send failed')
  }
  this.sentRaw.push(raw)
}
```

Replace the old `drops queued sends when the socket is not open` test with these behavioral assertions in `websocket.test.ts`:

```ts
it('reports rejection while connecting and acceptance while open', () => {
  wsManager.connect()
  expect(wsManager.sendCollabUpdate('document-a', 'YQ==')).toBe(false)
  expect(FakeWebSocket.last.sentRaw).toHaveLength(0)

  FakeWebSocket.last.openNow()
  expect(wsManager.sendCollabUpdate('document-a', 'YQ==')).toBe(true)
  expect(FakeWebSocket.last.sentOfType('collab.update')).toHaveLength(1)
})

it('returns false and closes when the platform send throws', () => {
  const socket = connectOpen()
  socket.failNextSend = true

  expect(wsManager.sendCollabUpdate('document-a', 'YQ==')).toBe(false)
  expect(socket.closeCount).toBe(1)
})
```

- [ ] **Step 2: Run the WebSocket test and confirm red**

Run:

```bash
cd app
npx vitest run test/websocket.test.ts
```

Expected: both return-value assertions receive `undefined`, and the throwing fake send escapes instead of returning `false`.

- [ ] **Step 3: Implement the boolean transport contract**

Replace `send()` and update the collaboration wrapper:

```ts
send(type: string, data: unknown): boolean {
  const socket = this.ws
  if (!socket || socket.readyState !== WebSocket.OPEN) return false
  try {
    socket.send(JSON.stringify({ type, data }))
    return true
  } catch {
    try {
      socket.close()
    } catch {
      // The platform may already be closing the socket.
    }
    return false
  }
}

sendCollabUpdate(documentId: string, updateBase64: string): boolean {
  return this.send('collab.update', { document_id: documentId, update: updateBase64 })
}
```

Callers such as subscribe, typing, presence, awareness, and huddles may ignore the returned boolean. Only durable collaboration updates consume it in Task 5.

- [ ] **Step 4: Run focused tests, typecheck, and commit**

Run:

```bash
cd app
npx vitest run test/websocket.test.ts
npm run typecheck
```

Expected: WebSocket tests pass, including existing desired-subscription behavior, and TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/websocket.ts app/test/helpers.ts app/test/websocket.test.ts
git commit -m "fix: report realtime send acceptance"
```

---

### Task 5: Make collaboration durability explicit and recover the disconnect race

**Files:**
- Modify: `app/src/lib/collab/provider.ts`
- Modify: `app/test/collab.test.ts`

**Interfaces:**
- Consumes: Task 4's boolean `sendCollabUpdate()`, existing `wsManager.onStatus()`, `wsManager.getConnectionId()`, `wsManager.roomAccess()`, `collabApi.state()`, and `collabApi.append(): Promise<ApiResponse<{seq: number}>>`.
- Produces: `ProviderStatus` including `'saving'`, `isProviderEditable(status): boolean`, `provider.hasPendingChanges: boolean`, and `provider.retry(): void`. The provider retains only merged Yjs bytes in memory; it exposes no document body or pending payload.

- [ ] **Step 1: Add failing online acknowledgement and fail-closed tests**

Update the default `/updates` response in `app/test/collab.test.ts` to honor the real contract:

```ts
net = mockFetch((req) => {
  if (req.path.includes('/state')) return emptyState()
  if (req.path.includes('/updates')) return envelope(201, { data: { seq: 1 } })
  return ok({})
})
```

Import `envelope` from `./helpers`, emit a real connection id before joins, and add these cases:

```ts
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
```

Update the existing own-echo test to emit `hello {connection_id: 'connection-a'}` and use `origin_conn: 'connection-a'`; actor id alone is not sufficient because the same user can edit from multiple tabs.

- [ ] **Step 2: Add failing reconnect recovery, retry, and async-race tests**

Add these cases to `collab.test.ts`:

```ts
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
```

Add these deferred-response cases:

```ts
it('ignores a state response released after destroy', async () => {
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
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

it('lets revocation win over a delayed recovery append', async () => {
  let release!: () => void
  const gate = new Promise<void>((resolve) => { release = resolve })
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
```

- [ ] **Step 3: Run the collaboration test and confirm red**

Run:

```bash
cd app
npx vitest run test/collab.test.ts
```

Expected: `saving`, `hasPendingChanges`, `retry()`, and send acceptance handling do not exist; the old provider reports `synced` immediately and loses the update on reconnect.

- [ ] **Step 4: Add provider status, pending state, connection subscription, and public retry**

Extend the provider contract and fields:

```ts
export type ProviderStatus =
  | 'connecting'
  | 'saving'
  | 'synced'
  | 'read-only'
  | 'revoked'
  | 'error'

export function isProviderEditable(status: ProviderStatus): boolean {
  return status === 'synced' || status === 'saving'
}

private pendingUpdate: Uint8Array | null = null
private pendingVersion = 0
private pendingSocketAcks = 0
private needsHttpRecovery = false
private recoveryEpoch = 0
private recoveryInFlight: Promise<void> | null = null
private offConnection: (() => void) | null = null
```

Add the public surfaces:

```ts
get hasPendingChanges(): boolean {
  return this.pendingUpdate !== null
}

retry(): void {
  if (this.destroyed || this.status === 'revoked') return
  this.setStatus('connecting')
  if (wsManager.isConnected() && wsManager.roomAccess(this.documentId)) {
    this.startRecovery()
  } else {
    wsManager.connect()
  }
}
```

In the constructor, subscribe to connection status. A disconnect invalidates the active recovery, converts every unacknowledged socket send into HTTP recovery work, and disables editing:

```ts
this.offConnection = wsManager.onStatus((connected) => {
  if (this.destroyed || this.status === 'revoked') return
  if (connected) return
  this.recoveryEpoch += 1
  this.recoveryInFlight = null
  this.pendingSocketAcks = 0
  if (this.pendingUpdate) this.needsHttpRecovery = true
  this.setStatus('connecting')
})
```

In `destroy()`, set `destroyed`, increment `recoveryEpoch`, and unregister `offConnection` before touching awareness or leaving the room. In the revoked branch, increment the epoch before setting `revoked`. Make `setStatus()` return without callbacks when destroyed. Make `handleRoom()` return immediately for every later event once `status === 'revoked'`, so a queued update cannot mutate the revoked document.

- [ ] **Step 5: Merge before sending and acknowledge only own-connection echoes**

Replace the small-update branch with this ordering:

```ts
private mergePending(update: Uint8Array): void {
  this.pendingUpdate = this.pendingUpdate
    ? Y.mergeUpdates([this.pendingUpdate, update])
    : update
  this.pendingVersion += 1
}

private async publish(update: Uint8Array): Promise<void> {
  if (!this.canWrite || this.destroyed || this.status === 'revoked') return
  this.mergePending(update)
  const encoded = toBase64(update)

  if (this.status === 'error') {
    this.needsHttpRecovery = true
    return
  }

  if (this.status === 'connecting') {
    this.needsHttpRecovery = true
    this.setStatus('connecting')
    return
  }

  if (this.recoveryInFlight) {
    // A normal large online append is already saving. Keep the editor live;
    // its version-stable flush loop will include this newer update.
    this.needsHttpRecovery = true
    this.setStatus('saving')
    return
  }

  if (this.needsHttpRecovery) {
    this.setStatus('saving')
    this.startRecovery(false)
    return
  }

  if (encoded.length <= MAX_FRAME_UPDATE_BYTES) {
    this.setStatus('saving')
    if (wsManager.sendCollabUpdate(this.documentId, encoded)) {
      this.pendingSocketAcks += 1
      return
    }
    this.pendingSocketAcks = 0
    this.needsHttpRecovery = true
    this.setStatus('connecting')
    return
  }

  this.needsHttpRecovery = true
  this.setStatus('saving')
  this.startRecovery(false)
}
```

Pass `originConn` into the durable acknowledgement path after applying every room update:

```ts
private acknowledgeOwnEcho(originConn: string): void {
  if (!originConn || originConn !== wsManager.getConnectionId()) return
  if (this.pendingSocketAcks === 0 || this.needsHttpRecovery) return
  this.pendingSocketAcks -= 1
  if (this.pendingSocketAcks !== 0) return
  this.pendingUpdate = null
  this.setStatus(this.canWrite ? 'synced' : 'read-only')
}
```

Call `acknowledgeOwnEcho()` only after `Y.applyUpdate()` and sequence recording succeed. A remote tab owned by the same user cannot clear the pending update because its `origin_conn` differs.

Make that ordering explicit by returning success from `applyRemote()` and using it in the room-event branch:

```ts
case 'update':
  if (this.applyRemote(e.seq, e.update)) this.acknowledgeOwnEcho(e.originConn)
  break

private applyRemote(seq: number, updateBase64: string): boolean {
  try {
    Y.applyUpdate(this.doc, fromBase64(updateBase64), LOCAL_ORIGIN)
  } catch {
    this.startRecovery(true)
    return false
  }
  this.recordAppliedSeq(seq)
  return true
}
```

- [ ] **Step 6: Implement guarded catch-up followed by durable HTTP flush**

Split the old `catchUp()` into three operations: `startRecovery(catchUpFirst = true)`, `catchUp(epoch)`, and `flushPending(epoch)`. Use this exact lifecycle rule after every await:

```ts
private active(epoch: number): boolean {
  return !this.destroyed && this.status !== 'revoked' && epoch === this.recoveryEpoch
}
```

`startRecovery()` owns one epoch and preserves `connecting` for reconnect work while allowing `saving` for a normal online large update:

```ts
private startRecovery(catchUpFirst = true): void {
  if (this.recoveryInFlight || this.destroyed || this.status === 'revoked') return
  if (catchUpFirst) this.setStatus('connecting')
  const epoch = ++this.recoveryEpoch
  const operation = (async () => {
    try {
      if (catchUpFirst) await this.catchUp(epoch)
      if (!this.active(epoch)) return
      await this.flushPending(epoch)
      if (!this.active(epoch) || this.pendingUpdate) return
      this.needsHttpRecovery = false
      this.setStatus(this.canWrite ? 'synced' : 'read-only')
      this.publishAwareness()
    } catch (error) {
      if (!this.active(epoch)) return
      this.setStatus(
        'error',
        error instanceof Error ? error.message : 'An edit could not be saved',
      )
    } finally {
      if (this.recoveryEpoch === epoch) this.recoveryInFlight = null
    }
  })()
  this.recoveryInFlight = operation
}
```

`catchUp(epoch)` retains the current paginated REST algorithm, but it checks `active(epoch)` immediately after every `collabApi.state()` await and before each Yjs application, sequence change, or callback. It does not set final screen status itself.

`flushPending(epoch)` uses a version-stable loop:

```ts
private async flushPending(epoch: number): Promise<void> {
  while (this.pendingUpdate) {
    const update = this.pendingUpdate
    const version = this.pendingVersion
    const res = await collabApi.append(this.documentId, toBase64(update))
    if (!this.active(epoch)) return
    const seq = res.data?.seq
    if (!Number.isSafeInteger(seq) || seq <= 0) {
      throw new Error('The server did not acknowledge the edit sequence.')
    }

    this.recordAppliedSeq(seq)
    if (this.watermark < seq) await this.catchUp(epoch)
    if (!this.active(epoch)) return
    if (this.watermark < seq) {
      throw new Error('The saved edit could not be reconciled locally.')
    }

    if (this.pendingVersion === version) {
      this.pendingUpdate = null
      this.pendingSocketAcks = 0
      this.needsHttpRecovery = false
    } else {
      // A programmatic local update arrived while the UI was fail-closed.
      // Resend the merged value; duplicate Yjs bytes are state-idempotent.
      this.pendingSocketAcks = 0
      this.needsHttpRecovery = true
    }
  }
}
```

For this task, preserve the old contiguous sequence logic behind the name consumed by `flushPending()`:

```ts
private recordAppliedSeq(seq: number): void {
  this.advanceWatermark(seq)
}
```

Task 6 replaces that wrapper with the callback-emitting implementation and unifies catch-up sequence advancement.

On `collab.joined`, update access/head and call `startRecovery(true)`; on `resumed`, set `connecting` and wait for the following joined acknowledgement. Initial load therefore uses the same guarded recovery path as reconnect.

- [ ] **Step 7: Run provider tests, backend protocol regressions, and commit**

Run:

```bash
cd app
npx vitest run test/collab.test.ts test/websocket.test.ts
npm run typecheck
cd ../backend
go test ./internal/ws ./internal/collab
```

Expected: collaboration and WebSocket tests pass; the provider stays noneditable through failed recovery; retry clears the same pending update only after a valid sequence; backend protocol tests remain green; TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/collab/provider.ts app/test/collab.test.ts
git commit -m "fix: recover unacknowledged collaborative edits"
```

---

### Task 6: Give projections the contiguous provider watermark and finish screen state

**Files:**
- Modify: `app/src/lib/collab/provider.ts`
- Modify: `app/src/screens/CollabDocumentScreen.tsx`
- Test: `app/test/collab.test.ts`
- Test: `app/test/projection.test.ts`
- Test: `app/test/sheet.test.ts`

**Interfaces:**
- Consumes: Task 5's `recordAppliedSeq`, recovery append response, `ProviderStatus`, `isProviderEditable()`, and `retry()`.
- Produces: required `ProviderOptions.onProjectionSeq(seq: number): void`; the callback emits only a strictly advanced contiguous watermark. The screen passes that value to document/sheet/design projection extractors and never assigns descriptor `head_seq` to projection state.

- [ ] **Step 1: Add failing contiguous-watermark behavior tests**

Make `makeProvider()` accept an optional projection callback and always satisfy the required option:

```ts
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
```

Add these tests to `collab.test.ts`:

```ts
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
```

In the existing own-echo test, replace its provider construction with:

```ts
const projected: number[] = []
const { doc, provider } = makeProvider(undefined, undefined, (seq) => projected.push(seq))
```

Immediately after that test emits the own-connection echo and awaits `flush()`, add:

```ts
expect(projected).toEqual([1])
```

Add this recovery-gap case. It proves an append acknowledgement cannot jump over an unapplied sequence:

```ts
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
```

- [ ] **Step 2: Run projection-focused tests and confirm red**

Run:

```bash
cd app
npx vitest run test/collab.test.ts test/projection.test.ts test/sheet.test.ts
```

Expected: provider construction fails because `onProjectionSeq` is absent from the interface, or the callback arrays stay empty; current screen behavior is still pinned to descriptor `head_seq`.

- [ ] **Step 3: Unify all sequence advancement behind one callback**

Make `onProjectionSeq` required in `ProviderOptions`, save it on the provider, and rename the old gap set to `pendingSeqs` so it cannot be confused with local pending Yjs bytes:

```ts
onProjectionSeq: (seq: number) => void

private pendingSeqs = new Set<number>()
private readonly onProjectionSeq: (seq: number) => void
```

Replace direct watermark writes with these two methods:

```ts
private emitProjectionAdvance(previous: number): void {
  if (!this.destroyed && this.status !== 'revoked' && this.watermark > previous) {
    this.onProjectionSeq(this.watermark)
  }
}

private recordAppliedSeq(seq: number): void {
  if (!Number.isSafeInteger(seq) || seq <= this.watermark) return
  const previous = this.watermark
  this.pendingSeqs.add(seq)
  while (this.pendingSeqs.has(this.watermark + 1)) {
    this.pendingSeqs.delete(this.watermark + 1)
    this.watermark += 1
  }
  this.headSeq = Math.max(this.headSeq, seq)
  this.emitProjectionAdvance(previous)
  if (this.pendingSeqs.size > 0 && seq - this.watermark > 64 && !this.recoveryInFlight) {
    this.startRecovery(true)
  }
}

private advanceThrough(throughSeq: number): void {
  if (!Number.isSafeInteger(throughSeq) || throughSeq < this.watermark) return
  const previous = this.watermark
  this.watermark = throughSeq
  for (const seq of [...this.pendingSeqs]) {
    if (seq <= this.watermark) this.pendingSeqs.delete(seq)
  }
  while (this.pendingSeqs.has(this.watermark + 1)) {
    this.pendingSeqs.delete(this.watermark + 1)
    this.watermark += 1
  }
  this.emitProjectionAdvance(previous)
}
```

Call `recordAppliedSeq()` after every successfully applied room update and for the validated HTTP append result. Call `advanceThrough(state.through_seq)` after each contiguous state page. Never call the projection callback with `headSeq`.

- [ ] **Step 4: Make the screen consume only the provider watermark**

In `CollabDocumentScreen.tsx`, replace `seq` with state plus a synchronous ref:

```ts
const [projectionSeq, setProjectionSeq] = useState(0)
const projectionSeqRef = useRef(0)

const acceptProjectionSeq = useCallback((next: number) => {
  if (next <= projectionSeqRef.current) return
  projectionSeqRef.current = next
  setProjectionSeq(next)
}, [])

useEffect(() => {
  projectionSeqRef.current = 0
  setProjectionSeq(0)
}, [documentId])
```

Pass `onProjectionSeq: acceptProjectionSeq` to `new CollabProvider` and include the stable callback in that effect's dependency list. Remove `setSeq(p?.head_seq ?? 0)` from the descriptor request; that response now does only the strict `head_seq > projection_seq` repair-nonce check. Replace the nearby descriptor-source comment so it states that the provider watermark owns projection sequence and the descriptor gap is only a repair hint.

Every sheet/design projection created inside a timer or unmount cleanup must read `projectionSeqRef.current` at execution time:

```ts
const scheduleProjection = useCallback(() => {
  if (projectTimer.current) clearTimeout(projectTimer.current)
  projectTimer.current = setTimeout(() => {
    const seq = projectionSeqRef.current
    publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
  }, 2000)
}, [publish, fileType, sheet, design])

flushRef.current = () => {
  if (!fileId || fileType === 'document') return
  const seq = projectionSeqRef.current
  publish(fileType === 'spreadsheet' ? extractSheet(sheet, seq) : extractDesign(design, seq))
}
```

The sheet/design repair timer also reads `projectionSeqRef.current`. Pass `<Editor seq={projectionSeq} />`; `Editor.web.tsx` already maintains its own latest `seqRef`, so no editor implementation changes are needed.

- [ ] **Step 5: Finish saving/editability/retry presentation for all three surfaces**

Use the single pure helper for each `editable` prop:

```ts
const editable = isProviderEditable(status)

<Grid model={sheet} editable={editable} revision={revision} onEdit={scheduleProjection} />
<Canvas model={design} editable={editable} revision={revision} onEdit={scheduleProjection} />
<Editor
  doc={doc}
  awareness={providerRef.current!.awareness}
  editable={editable}
  user={{ name: identity.name, color: identity.color }}
  onProject={publish}
  seq={projectionSeq}
  fileId={fileId}
  catchUp={projectNonce}
/>
```

Add the saving subtitle and a real retry path:

```ts
case 'saving':
  return 'Saving changes…'
case 'error':
  return 'Changes not saved'

<ErrorState
  message={detail ?? 'Changes have not been saved.'}
  onRetry={() => providerRef.current?.retry()}
  retryLabel="Retry saving"
/>
```

Update the screen's introductory durability comment to say that local updates are pending until an own echo or HTTP append acknowledgement; it must no longer claim every keystroke is already durable.

Add this pure behavior test to `collab.test.ts`:

```ts
it('edits only in synced and saving states', () => {
  expect(isProviderEditable('synced')).toBe(true)
  expect(isProviderEditable('saving')).toBe(true)
  expect(isProviderEditable('connecting')).toBe(false)
  expect(isProviderEditable('error')).toBe(false)
  expect(isProviderEditable('read-only')).toBe(false)
  expect(isProviderEditable('revoked')).toBe(false)
})
```

Do not add a source-text regex test. The helper test verifies the state contract, required `onProjectionSeq` makes a missing screen connection a type error, and the final browser gate verifies the rendered surfaces.

- [ ] **Step 6: Retain extractor sequence regressions, run green, and commit**

The document and sheet extractor tests already assert their input sequence. In the existing design projection test in `app/test/sheet.test.ts`, add:

```ts
expect(p.seq).toBe(3)
```

Run:

```bash
cd app
npx vitest run test/collab.test.ts test/projection.test.ts test/sheet.test.ts
npm run typecheck
```

Expected: focused tests pass; gap callbacks are `[1, 3]`; append seq does not jump a gap; document, sheet, and design extractors receive the provider watermark; TypeScript reports no errors.

Commit:

```bash
git add app/src/lib/collab/provider.ts app/src/screens/CollabDocumentScreen.tsx app/test/collab.test.ts app/test/projection.test.ts app/test/sheet.test.ts
git commit -m "fix: project the contiguous collaboration sequence"
```

---

## Phase 1 Verification Gate

- [ ] From `app`, run `npm test`; expected result is every Vitest file and test passing with zero failures.
- [ ] From `app`, run `npm run typecheck`; expected result is exit code 0 with no TypeScript diagnostics.
- [ ] From `app`, run `npm run build`; expected result is a successful Expo web export.
- [ ] From `backend`, run `go test ./internal/ws ./internal/collab`; expected result is both packages passing.
- [ ] Inspect `git diff --check`; expected result is no whitespace errors.
- [ ] Inspect `git status --short`; expected result is no uncommitted implementation files after the six task commits.
- [ ] Start the existing local app/backend stack, sign in as account A, open a channel/thread/Drive object, log out, sign in as account B, and verify none of A's workspace, channel, thread, message, user, Drive, presence, or custom-emoji data appears.
- [ ] In browser developer tools, block or close the app WebSocket while a document is open. Verify the header stops saying `All changes saved`, all three collaborative surfaces are noneditable while reconnecting, and a race-window edit is retained.
- [ ] Restore the socket. Verify the network order is collaboration state GET before update POST, the header returns to `All changes saved` only after the POST receives a valid sequence, and the pending edit remains in the document.
- [ ] Force the recovery POST to return 503. Verify the document shows `Changes have not been saved`, remains noneditable, and `Retry saving` sends the retained update again and restores `All changes saved` only after success.
- [ ] For a document, spreadsheet, and design file, make a second edit after initial load and verify the projection request sequence increases to the provider's latest contiguous sequence.
- [ ] Review `git diff 8be79db...HEAD -- app`; expected scope is only the client/session/collaboration files listed in this plan, with no backend behavior or unrelated UI changes.

## Completion Handoff

Phase 1 is accepted only after every task's red and green output is recorded in the execution ledger, all automated commands above pass from a fresh run, the account-switch and browser recovery checks pass, and the worktree is clean. Then use `superpowers:requesting-code-review`, resolve any correctness findings, and use `superpowers:verification-before-completion` before claiming Phase 1 complete or beginning Phase 2.
