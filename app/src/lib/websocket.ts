import { useAuthStore } from '../stores/authStore'
import { useMessageStore } from '../stores/messageStore'
import { useChannelStore } from '../stores/channelStore'
import { useWorkspaceStore } from '../stores/workspaceStore'
import { useUiStore, type ConnectionStatus } from '../stores/uiStore'
import { channelApi } from '../api/channels'
import { messageApi } from '../api/messages'
import { notificationApi } from '../api/notifications'
import { WS_BASE_URL } from '../config'
import { normalizeMessage } from './types'
import type { Channel, PresenceStatus, WireMessage } from './types'

export type WSEventHandler = (data: unknown) => void
export type ResyncReason = 'seq-gap' | 'reconnect' | 'revoked' | 'manual'

/** Server → client frame. `seq` is monotonic per connection on EVERY frame. */
interface InboundFrame {
  type: string
  seq?: number
  data?: unknown
}

const TYPING_TTL_MS = 4000
const MAX_RECONNECT_DELAY_MS = 30_000
const INITIAL_RECONNECT_DELAY_MS = 1000
/** Floor between two REST resyncs, so a burst of gaps cannot stampede the API. */
const RESYNC_COOLDOWN_MS = 2000

function record(data: unknown): Record<string, unknown> | null {
  return data && typeof data === 'object' ? (data as Record<string, unknown>) : null
}

function str(d: Record<string, unknown> | null, key: string): string | undefined {
  const v = d?.[key]
  return typeof v === 'string' ? v : undefined
}

/**
 * Recognises a channel object in an event payload. The relay requires certain
 * top-level routing keys (`channel_id`, `workspace_id`), so a publisher may send
 * either a bare channel or a wrapper — accept both, and refetch when it is
 * neither.
 */
function asChannel(data: unknown): Channel | null {
  const d = record(data)
  if (!d) return null
  if (typeof d.id === 'string' && typeof d.workspace_id === 'string' && typeof d.type === 'string') {
    const ch = d as unknown as Channel
    // unread_count is computed per caller and absent from most payloads.
    return typeof ch.unread_count === 'number' ? ch : { ...ch, unread_count: 0 }
  }
  if (d.channel) return asChannel(d.channel)
  return null
}

class WebSocketManager {
  private ws: WebSocket | null = null
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectDelay = INITIAL_RECONNECT_DELAY_MS
  private connected = false
  /** Set before an application-initiated close so `onclose` does not reconnect. */
  private intentionalClose = false
  private everConnected = false

  private connectionId: string | null = null
  private lastSeq = 0

  private handlers: Map<string, WSEventHandler[]> = new Map()
  /** Channels the app wants subscribed. */
  private desiredChannels: Set<string> = new Set()
  /** Channels the server has acknowledged with a `subscribed` frame. */
  private confirmedChannels: Set<string> = new Set()
  private typingTimers: Map<string, ReturnType<typeof setTimeout>> = new Map()

  private statusListeners: Set<(connected: boolean) => void> = new Set()
  private resyncListeners: Set<(reason: ResyncReason) => void> = new Set()
  private resyncInFlight = false
  private lastResyncAt = 0

  // ---------------------------------------------------------------------------
  // Lifecycle
  // ---------------------------------------------------------------------------

  connect() {
    const token = useAuthStore.getState().accessToken
    if (!token) return
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return

    this.intentionalClose = false
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.setStatus(this.everConnected ? 'reconnecting' : 'connecting')

    // `?token=` is accepted here (and only here plus file download) because the
    // WebSocket constructor cannot set an Authorization header.
    const socket = new WebSocket(`${WS_BASE_URL}?token=${encodeURIComponent(token)}`)
    this.ws = socket

    socket.onopen = () => {
      this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
      this.lastSeq = 0
      this.confirmedChannels.clear()
      this.setConnected(true)
      // Subscriptions are per connection; re-assert them all. The server acks
      // each with a `subscribed` frame, so nothing races the upgrade any more.
      this.desiredChannels.forEach((chId) => this.send('subscribe', { channel_id: chId }))
      if (this.everConnected) {
        // Anything published while the socket was down is gone — there is no
        // server-side replay, so the only recovery is a REST refetch.
        void this.resync('reconnect')
      }
      this.everConnected = true
    }

    socket.onmessage = (event) => {
      let frame: InboundFrame
      try {
        frame = JSON.parse(String((event as MessageEvent).data)) as InboundFrame
      } catch {
        return
      }
      if (!frame || typeof frame.type !== 'string') return
      this.trackSeq(frame.seq)
      this.dispatch(frame.type, frame.data)
    }

    socket.onclose = () => {
      if (this.ws !== socket) return // superseded by a newer socket
      this.connectionId = null
      this.confirmedChannels.clear()
      this.setConnected(false)
      if (this.intentionalClose) {
        this.ws = null
        this.setStatus('idle')
        return
      }
      this.setStatus('offline')
      this.scheduleReconnect()
    }

    socket.onerror = () => {
      // onerror is always followed by onclose; closing here just makes it prompt.
      try {
        socket.close()
      } catch {
        /* already closing */
      }
    }
  }

  /**
   * Closes the socket without triggering a reconnect.
   *
   * The intentional-close flag is the whole point: previously `disconnect()`
   * cleared the timer, closed, and nulled the ref — but the socket's `onclose`
   * still ran `scheduleReconnect()`, so a zombie socket came back ~1s later.
   */
  disconnect() {
    this.intentionalClose = true
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.clearTypingTimers()
    const socket = this.ws
    this.ws = null
    if (socket) {
      try {
        socket.close()
      } catch {
        /* already closed */
      }
    }
    this.desiredChannels.clear()
    this.confirmedChannels.clear()
    this.connectionId = null
    this.lastSeq = 0
    // A deliberate teardown is a clean slate: the next connect() is a first
    // connect, not a reconnect, so it must not report "reconnecting" or fire a
    // gap-recovery resync. Unintentional closes never come through here.
    this.everConnected = false
    this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
    this.setConnected(false)
    this.setStatus('idle')
  }

  /** Full teardown for sign-out: drops listeners and reconnect history too. */
  reset() {
    this.disconnect()
    this.handlers.clear()
    this.resyncListeners.clear()
    this.everConnected = false
    this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
  }

  private scheduleReconnect() {
    if (this.reconnectTimer || this.intentionalClose) return
    if (!useAuthStore.getState().accessToken) return
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, MAX_RECONNECT_DELAY_MS)
      this.connect()
    }, this.reconnectDelay)
  }

  // ---------------------------------------------------------------------------
  // Status
  // ---------------------------------------------------------------------------

  isConnected() {
    return this.connected
  }

  /** Current connection id from the `hello` frame — useful for support logs. */
  getConnectionId() {
    return this.connectionId
  }

  onStatus(cb: (connected: boolean) => void) {
    this.statusListeners.add(cb)
    cb(this.connected)
    return () => {
      this.statusListeners.delete(cb)
    }
  }

  /** Notified after a gap/reconnect refetch so a screen can reload its own view. */
  onResync(cb: (reason: ResyncReason) => void) {
    this.resyncListeners.add(cb)
    return () => {
      this.resyncListeners.delete(cb)
    }
  }

  private setConnected(v: boolean) {
    if (this.connected === v) return
    this.connected = v
    this.statusListeners.forEach((l) => l(v))
  }

  private setStatus(status: ConnectionStatus, error: string | null = null) {
    useUiStore.getState().setConnection(status, error)
  }

  // ---------------------------------------------------------------------------
  // Sequence / resync
  // ---------------------------------------------------------------------------

  private trackSeq(seq: number | undefined) {
    if (typeof seq !== 'number' || seq <= 0) return
    if (this.lastSeq !== 0 && seq !== this.lastSeq + 1) {
      // Backpressure dropped frames. Nothing is replayed server-side, so the
      // only correct response is to refetch.
      this.lastSeq = seq
      void this.resync('seq-gap')
      return
    }
    this.lastSeq = seq
  }

  /** Refetches everything realtime is responsible for keeping fresh. */
  async resync(reason: ResyncReason = 'manual'): Promise<void> {
    const now = Date.now()
    if (this.resyncInFlight || now - this.lastResyncAt < RESYNC_COOLDOWN_MS) return
    this.resyncInFlight = true
    this.lastResyncAt = now

    try {
      const workspaceId = useWorkspaceStore.getState().activeWorkspace?.id
      if (workspaceId) {
        try {
          const res = await channelApi.list(workspaceId)
          useChannelStore.getState().setChannels(res.data ?? [])
        } catch {
          /* keep the stale list rather than blanking the sidebar */
        }
      }

      const loaded = Object.keys(useMessageStore.getState().messages)
      await Promise.all(
        loaded.map(async (channelId) => {
          try {
            const res = await messageApi.list(channelId)
            useMessageStore
              .getState()
              .setMessages(channelId, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
          } catch {
            /* one channel failing must not abort the rest */
          }
        }),
      )

      try {
        const res = await notificationApi.unreadCount()
        useUiStore.getState().setUnread(res.data?.count ?? 0)
      } catch {
        /* badge stays stale */
      }
    } finally {
      this.resyncInFlight = false
    }

    this.resyncListeners.forEach((l) => l(reason))
  }

  // ---------------------------------------------------------------------------
  // Sending
  // ---------------------------------------------------------------------------

  send(type: string, data: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, data }))
    }
  }

  subscribe(channelId: string) {
    if (this.desiredChannels.has(channelId)) return
    this.desiredChannels.add(channelId)
    this.send('subscribe', { channel_id: channelId })
  }

  unsubscribe(channelId: string) {
    if (!this.desiredChannels.delete(channelId)) return
    this.confirmedChannels.delete(channelId)
    this.send('unsubscribe', { channel_id: channelId })
  }

  /**
   * Declarative form of subscribe/unsubscribe: only the difference is sent.
   * Calling `subscribe` for every channel on each list refresh churned the whole
   * subscription set (and a Postgres membership query per channel) every time
   * the array identity changed.
   */
  setSubscriptions(channelIds: string[]) {
    const next = new Set(channelIds)
    this.desiredChannels.forEach((id) => {
      if (!next.has(id)) this.unsubscribe(id)
    })
    next.forEach((id) => this.subscribe(id))
  }

  /** True once the server has acknowledged the subscription on this connection. */
  isSubscribed(channelId: string) {
    return this.confirmedChannels.has(channelId)
  }

  sendTyping(channelId: string) {
    this.send('typing.start', { channel_id: channelId })
  }

  sendTypingStop(channelId: string) {
    this.send('typing.stop', { channel_id: channelId })
  }

  setPresence(status: PresenceStatus) {
    this.send('presence.update', { status })
  }

  on(type: string, handler: WSEventHandler) {
    const handlers = this.handlers.get(type) || []
    handlers.push(handler)
    this.handlers.set(type, handlers)
    return () => {
      this.handlers.set(type, (this.handlers.get(type) || []).filter((h) => h !== handler))
    }
  }

  // ---------------------------------------------------------------------------
  // Typing timers
  // ---------------------------------------------------------------------------

  private stopTypingTimer(key: string) {
    const t = this.typingTimers.get(key)
    if (t) {
      clearTimeout(t)
      this.typingTimers.delete(key)
    }
  }

  private clearTypingTimers() {
    this.typingTimers.forEach((t) => clearTimeout(t))
    this.typingTimers.clear()
    useUiStore.getState().clearTyping()
  }

  // ---------------------------------------------------------------------------
  // Dispatch
  // ---------------------------------------------------------------------------

  private dispatch(type: string, data: unknown) {
    const ms = useMessageStore.getState()
    const ui = useUiStore.getState()
    const cs = useChannelStore.getState()
    const d = record(data)

    switch (type) {
      // --- connection ------------------------------------------------------
      case 'hello': {
        this.connectionId = str(d, 'connection_id') ?? null
        this.setStatus('connected')
        break
      }
      case 'pong':
        // The server drives liveness with WebSocket ping frames now; nothing to do.
        break
      case 'subscribed': {
        const channelId = str(d, 'channel_id')
        if (channelId) this.confirmedChannels.add(channelId)
        break
      }
      case 'unsubscribed': {
        const channelId = str(d, 'channel_id')
        if (!channelId) break
        this.confirmedChannels.delete(channelId)
        if (str(d, 'reason') === 'revoked') {
          // Access was withdrawn (removed from the channel, archived, deleted).
          this.desiredChannels.delete(channelId)
          cs.removeChannel(channelId)
          ms.clearChannel(channelId)
          void this.resync('revoked')
        }
        break
      }
      case 'error': {
        const code = str(d, 'code') ?? 'UNKNOWN'
        const message = str(d, 'message') ?? 'realtime error'
        // Subscribe rejections used to be dropped silently, so a channel simply
        // never went live and nothing said why.
        console.warn(`[ws] ${code}: ${message}`)
        this.setStatus(this.connected ? 'connected' : 'offline', code)
        break
      }

      // --- messages --------------------------------------------------------
      case 'message.new': {
        const m = normalizeMessage(data as WireMessage)
        ms.addMessage(m.channel_id, m)
        break
      }
      case 'message.updated': {
        const m = normalizeMessage(data as WireMessage)
        ms.updateMessage(m.channel_id, m)
        break
      }
      case 'message.deleted': {
        const channelId = str(d, 'channel_id')
        const id = str(d, 'id') ?? str(d, 'message_id')
        if (channelId && id) ms.removeMessage(channelId, id)
        break
      }
      case 'reaction.added':
      case 'reaction.removed': {
        const channelId = str(d, 'channel_id')
        const messageId = str(d, 'message_id')
        const userId = str(d, 'user_id')
        const emoji = str(d, 'emoji')
        if (!channelId || !messageId || !userId || !emoji) break
        ms.applyReaction(
          channelId,
          { id: '', message_id: messageId, user_id: userId, emoji, created_at: '' },
          type === 'reaction.added',
        )
        break
      }

      // --- ephemeral -------------------------------------------------------
      case 'typing.indicator': {
        const channelId = str(d, 'channel_id')
        const userId = str(d, 'user_id')
        if (!channelId || !userId) break
        if (userId === useAuthStore.getState().user?.id) break
        const key = `${channelId}:${userId}`
        // `typing.stop` is now delivered as typing:false instead of an error frame.
        if (d?.typing === false) {
          this.stopTypingTimer(key)
          ui.removeTyping(channelId, userId)
          break
        }
        ui.addTyping(channelId, userId)
        this.stopTypingTimer(key)
        this.typingTimers.set(
          key,
          setTimeout(() => {
            this.typingTimers.delete(key)
            useUiStore.getState().removeTyping(channelId, userId)
          }, TYPING_TTL_MS),
        )
        break
      }
      case 'presence.changed': {
        const userId = str(d, 'user_id')
        const status = str(d, 'status') as PresenceStatus | undefined
        if (userId && status) ui.setUserPresence(userId, status)
        break
      }
      case 'notification.new': {
        ui.setUnread(useUiStore.getState().unreadNotifications + 1)
        break
      }
      case 'unread.update': {
        const channelId = str(d, 'channel_id')
        const count = d?.unread_count
        if (channelId && typeof count === 'number') cs.setUnreadCount(channelId, count)
        break
      }

      // --- channel / membership -------------------------------------------
      case 'channel.created': {
        const ch = asChannel(data)
        const activeWorkspace = useWorkspaceStore.getState().activeWorkspace?.id
        if (ch && (!activeWorkspace || ch.workspace_id === activeWorkspace)) {
          cs.addChannel(ch)
          // A public channel is announced workspace-wide; subscribing is only
          // meaningful once we are a member, and the server rejects the rest
          // with a FORBIDDEN error frame, which is now visible.
          if (ch.type !== 'public') this.subscribe(ch.id)
        }
        break
      }
      case 'channel.updated': {
        const ch = asChannel(data)
        if (ch) {
          cs.upsertChannel(ch)
          if (ch.is_archived) this.unsubscribe(ch.id)
        } else {
          void this.resync('manual')
        }
        break
      }
      case 'member.joined': {
        const channelId = str(d, 'channel_id')
        const userId = str(d, 'user_id')
        if (channelId && userId === useAuthStore.getState().user?.id) {
          this.subscribe(channelId)
          void this.resync('manual')
        }
        break
      }
      case 'member.left': {
        const channelId = str(d, 'channel_id')
        const userId = str(d, 'user_id')
        if (channelId && userId === useAuthStore.getState().user?.id) {
          this.unsubscribe(channelId)
          cs.removeChannel(channelId)
          ms.clearChannel(channelId)
        }
        break
      }
    }

    const handlers = this.handlers.get(type)
    if (handlers) handlers.forEach((h) => h(data))
  }
}

export const wsManager = new WebSocketManager()
