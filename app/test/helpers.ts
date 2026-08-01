import { vi } from 'vitest'
import { useAuthStore } from '../src/stores/authStore'
import { useChannelStore } from '../src/stores/channelStore'
import { useDriveStore } from '../src/stores/driveStore'
import { useMessageStore } from '../src/stores/messageStore'
import { useUiStore } from '../src/stores/uiStore'
import { useUserStore } from '../src/stores/userStore'
import { useWorkspaceStore } from '../src/stores/workspaceStore'
import { clearDMRosterCache } from '../src/components/channel/dmRosterCache'
import { clearCustomEmojiCache } from '../src/components/message/customEmoji'
import { clearWorkspaceRoleCache } from '../src/screens/internal/useWorkspaceRole'
import type { Channel, Message, User } from '../src/lib/types'

export const API = 'http://api.test/api/v1'

// ---------------------------------------------------------------------------
// Stores
// ---------------------------------------------------------------------------

/**
 * The stores are plain `create(...)` singletons (no persist middleware, no
 * context), so `setState` on the data slice is a complete reset — the action
 * closures are untouched.
 */
export function resetStores(): void {
  useAuthStore.setState({
    accessToken: null,
    refreshToken: null,
    user: null,
    isAuthenticated: false,
    hydrated: false,
    persistError: null,
  })
  useChannelStore.setState({ channels: [], activeChannel: null })
  useMessageStore.setState({ messages: {}, cursors: {}, hasMore: {} })
  useDriveStore.getState().clear()
  useUserStore.getState().clear()
  useUiStore.getState().clear()
  useWorkspaceStore.getState().clear()
  clearDMRosterCache()
  clearCustomEmojiCache()
  clearWorkspaceRoleCache()
}

export function signIn(access = 'access-1', refresh = 'refresh-1', userId = 'u-self'): void {
  useAuthStore.setState({
    accessToken: access,
    refreshToken: refresh,
    isAuthenticated: true,
    user: { id: userId } as User,
  })
}

// ---------------------------------------------------------------------------
// Fixtures
// ---------------------------------------------------------------------------

export function makeMessage(id: string, channelId: string, over: Partial<Message> = {}): Message {
  return {
    id,
    channel_id: channelId,
    user_id: 'u-other',
    content: `body ${id}`,
    content_type: 'text',
    is_edited: false,
    is_deleted: false,
    reply_count: 0,
    is_pinned: false,
    is_scheduled: false,
    metadata: {},
    reactions: null,
    files: null,
    created_at: '2026-07-25T00:00:00Z',
    updated_at: '2026-07-25T00:00:00Z',
    ...over,
  }
}

export function makeChannel(id: string, over: Partial<Channel> = {}): Channel {
  return {
    id,
    workspace_id: 'ws-1',
    name: id,
    slug: id,
    description: '',
    type: 'public',
    topic: '',
    is_archived: false,
    creator_id: 'u-self',
    created_at: '2026-07-25T00:00:00Z',
    updated_at: '2026-07-25T00:00:00Z',
    unread_count: 0,
    ...over,
  }
}

// ---------------------------------------------------------------------------
// fetch boundary
// ---------------------------------------------------------------------------

export interface CapturedRequest {
  method: string
  /** Path with the API prefix stripped, e.g. `/auth/refresh`. */
  path: string
  headers: Record<string, string>
  body: unknown
}

export type RouteHandler = (req: CapturedRequest) => Response | Promise<Response>

export interface FetchMock {
  calls: CapturedRequest[]
  /** Requests whose path starts with `prefix`. */
  to(prefix: string): CapturedRequest[]
}

/**
 * Replaces the global `fetch` with `handler`; every call is recorded. Callers
 * are expected to restore the real `fetch` in an `afterEach`.
 */
export function mockFetch(handler: RouteHandler): FetchMock {
  const calls: CapturedRequest[] = []

  const impl = async (input: unknown, init: RequestInit = {}): Promise<Response> => {
    const url = String(input)
    const req: CapturedRequest = {
      method: init.method ?? 'GET',
      path: url.startsWith(API) ? url.slice(API.length) : url,
      headers: (init.headers ?? {}) as Record<string, string>,
      body: typeof init.body === 'string' ? safeParse(init.body) : init.body,
    }
    calls.push(req)
    return handler(req)
  }

  globalThis.fetch = vi.fn(impl) as unknown as typeof fetch
  return {
    calls,
    to: (prefix: string) => calls.filter((c) => c.path.startsWith(prefix)),
  }
}

function safeParse(text: string): unknown {
  try {
    return JSON.parse(text)
  } catch {
    return text
  }
}

/** `{data, meta, error}` envelope response, as httputil.JSON writes it. */
export function envelope(status: number, body: unknown): Response {
  return new Response(JSON.stringify(body), {
    status,
    headers: { 'Content-Type': 'application/json' },
  })
}

export function ok<T>(data: T, meta?: { cursor?: string; has_more: boolean }): Response {
  return envelope(200, meta ? { data, meta } : { data })
}

export function apiFailure(status: number, code: string, message = 'nope'): Response {
  return envelope(status, { error: { code, message } })
}

/** An nginx-style proxy error page — HTML, not JSON. */
export function htmlError(status: number): Response {
  return new Response(
    `<html>\r\n<head><title>${status} Bad Gateway</title></head>\r\n<body><center><h1>${status}</h1></center><hr><center>nginx/1.27.0</center></body>\r\n</html>`,
    { status, headers: { 'Content-Type': 'text/html' } },
  )
}

/** Lets queued microtasks (awaited fetch/JSON parses) drain under fake timers. */
export async function flush(times = 6): Promise<void> {
  for (let i = 0; i < times; i++) await Promise.resolve()
}

// ---------------------------------------------------------------------------
// WebSocket boundary
// ---------------------------------------------------------------------------

interface Framelike {
  type: string
  seq?: number
  data?: unknown
}

/**
 * Stand-in for the platform `WebSocket`. Only the surface `WebSocketManager`
 * uses is implemented: the four `on*` properties, `readyState`, `send`,
 * `close`, and the static readyState constants it compares against.
 */
export class FakeWebSocket {
  static readonly CONNECTING = 0
  static readonly OPEN = 1
  static readonly CLOSING = 2
  static readonly CLOSED = 3

  static instances: FakeWebSocket[] = []
  static reset(): void {
    FakeWebSocket.instances = []
  }
  static get last(): FakeWebSocket {
    const inst = FakeWebSocket.instances[FakeWebSocket.instances.length - 1]
    if (!inst) throw new Error('no WebSocket was constructed')
    return inst
  }

  readonly url: string
  readyState = FakeWebSocket.CONNECTING
  /** Raw frames handed to `send()`. */
  readonly sentRaw: string[] = []
  closeCount = 0

  onopen: (() => void) | null = null
  onmessage: ((e: { data: string }) => void) | null = null
  onclose: (() => void) | null = null
  onerror: (() => void) | null = null

  constructor(url: string) {
    this.url = url
    FakeWebSocket.instances.push(this)
  }

  send(raw: string): void {
    this.sentRaw.push(raw)
  }

  close(): void {
    this.closeCount++
    if (this.readyState === FakeWebSocket.CLOSED) return
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.()
  }

  // -- test drivers --------------------------------------------------------

  /** Completes the handshake. */
  openNow(): void {
    this.readyState = FakeWebSocket.OPEN
    this.onopen?.()
  }

  /** Delivers a server frame. */
  emit(frame: Framelike): void {
    this.onmessage?.({ data: JSON.stringify(frame) })
  }

  emitRaw(data: string): void {
    this.onmessage?.({ data })
  }

  /** A transport-initiated close (network drop) — must trigger a reconnect. */
  dropNow(): void {
    this.readyState = FakeWebSocket.CLOSED
    this.onclose?.()
  }

  /** Decoded `{type, data}` frames the client sent. */
  get sent(): { type: string; data: Record<string, unknown> }[] {
    return this.sentRaw.map((r) => JSON.parse(r) as { type: string; data: Record<string, unknown> })
  }

  sentOfType(type: string): { type: string; data: Record<string, unknown> }[] {
    return this.sent.filter((f) => f.type === type)
  }
}

export function installFakeWebSocket(): void {
  FakeWebSocket.reset()
  globalThis.WebSocket = FakeWebSocket as unknown as typeof WebSocket
}
