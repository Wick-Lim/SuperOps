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

/**
 * What happened to a collaboration room.
 *
 * `joined` carries the log head so a provider knows whether it is behind and
 * must fetch state over HTTP before it starts editing, and whether it may write
 * at all — the server decides that, and a client that assumed it could would
 * render an editable surface whose every keystroke is refused.
 *
 * `left` distinguishes the two reasons deliberately: `client` is "you closed
 * it", `revoked` is "your access was withdrawn", and the editor shows a
 * different thing for each. Collapsing them would tell somebody who was removed
 * from a document that they had left it.
 */
/**
 * A huddle event, delivered to a channel's SUBSCRIBERS.
 *
 * Subscribing requires read on the channel, so a stranger never learns a call
 * exists. Somebody who can read a public channel but has not subscribed finds
 * out from GET /channels/{id}/huddle instead — the pull path, which authorizes
 * per request.
 *
 * The server has published all three of these since huddles shipped and the
 * client dropped them on the floor: the dispatch switch is closed, so an
 * unhandled type reaches the generic handlers and nothing else.
 */
export type HuddleEvent =
  | { kind: 'started'; channelId: string; huddleId: string; startedBy: string }
  | { kind: 'ended'; channelId: string; huddleId: string; reason: string }
  | { kind: 'roster'; channelId: string; huddleId: string }

export type RoomEvent =
  | { kind: 'joined'; documentId: string; headSeq: number; canWrite: boolean }
  | { kind: 'left'; documentId: string; reason: 'client' | 'revoked' }
  | { kind: 'update'; documentId: string; seq: number; actorId: string; originConn: string; update: string }
  | { kind: 'awareness'; documentId: string; actorId: string; originConn: string; state: string }
  | { kind: 'compact'; documentId: string; headSeq: number; snapshotSeq: number }
  | { kind: 'project'; documentId: string; headSeq: number; projectionSeq: number }
  /** The socket dropped and came back; every room was re-joined from scratch. */
  | { kind: 'resumed'; documentId: string }

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

function num(d: Record<string, unknown> | null, key: string): number | undefined {
  const v = d?.[key]
  return typeof v === 'number' ? v : undefined
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
  private lifecycleGeneration = 0
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
  /**
   * Collaboration rooms this client wants to be in.
   *
   * A room is PER CONNECTION, exactly like a channel subscription, so a
   * reconnect must re-join them or the editor goes quietly one-way: it keeps
   * sending updates the server refuses ("join the document before sending
   * updates") while the document on screen stops changing. Tracking the desire
   * here rather than in the provider is what makes the re-join possible at all,
   * because only the manager sees `onopen`.
   */
  private desiredRooms: Set<string> = new Set()
  /** Rooms the server has acknowledged on THIS connection. */
  private joinedRooms: Map<string, { canWrite: boolean; headSeq: number }> = new Map()
  private roomListeners: Set<(e: RoomEvent) => void> = new Set()
  private huddleListeners: Set<(e: HuddleEvent) => void> = new Set()
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
    const generation = this.lifecycleGeneration

    socket.onopen = () => {
      if (generation !== this.lifecycleGeneration || this.ws !== socket) return
      this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
      this.lastSeq = 0
      this.confirmedChannels.clear()
      this.joinedRooms.clear()
      this.setConnected(true)
      // Subscriptions are per connection; re-assert them all. The server acks
      // each with a `subscribed` frame, so nothing races the upgrade any more.
      this.desiredChannels.forEach((chId) => this.send('subscribe', { channel_id: chId }))
      // Rooms are per connection too. Re-joining is not enough on its own —
      // the provider must also reconcile its watermark against the head the
      // server reports, because updates published while the socket was down are
      // not replayed — so it is told the room resumed.
      this.desiredRooms.forEach((docId) => {
        this.send('collab.join', { document_id: docId })
        this.emitRoom({ kind: 'resumed', documentId: docId })
      })
      if (this.everConnected) {
        // Anything published while the socket was down is gone — there is no
        // server-side replay, so the only recovery is a REST refetch.
        void this.resync('reconnect')
      }
      this.everConnected = true
    }

    socket.onmessage = (event) => {
      if (generation !== this.lifecycleGeneration || this.ws !== socket) return
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
      if (generation !== this.lifecycleGeneration || this.ws !== socket) return
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
      if (generation !== this.lifecycleGeneration || this.ws !== socket) return
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
    this.desiredRooms.clear()
    this.joinedRooms.clear()
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
    this.lifecycleGeneration += 1
    this.handlers.clear()
    this.statusListeners.clear()
    this.resyncListeners.clear()
    this.roomListeners.clear()
    this.huddleListeners.clear()
    this.resyncInFlight = false
    this.lastResyncAt = 0
    const socket = this.ws
    if (socket) {
      socket.onopen = null
      socket.onmessage = null
      socket.onerror = null
      socket.onclose = null
    }
    this.disconnect()
    this.everConnected = false
    this.reconnectDelay = INITIAL_RECONNECT_DELAY_MS
  }

  private scheduleReconnect() {
    if (this.reconnectTimer || this.intentionalClose) return
    if (!useAuthStore.getState().accessToken) return
    const generation = this.lifecycleGeneration
    this.reconnectTimer = setTimeout(() => {
      if (generation !== this.lifecycleGeneration) return
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
    const generation = this.lifecycleGeneration
    const now = Date.now()
    if (this.resyncInFlight || now - this.lastResyncAt < RESYNC_COOLDOWN_MS) return
    this.resyncInFlight = true
    this.lastResyncAt = now

    try {
      const workspaceId = useWorkspaceStore.getState().activeWorkspace?.id
      if (workspaceId) {
        try {
          const res = await channelApi.list(workspaceId)
          if (generation !== this.lifecycleGeneration) return
          useChannelStore.getState().setChannels(res.data ?? [])
        } catch {
          if (generation !== this.lifecycleGeneration) return
          /* keep the stale list rather than blanking the sidebar */
        }
      }

      const loaded = Object.keys(useMessageStore.getState().messages)
      await Promise.all(
        loaded.map(async (channelId) => {
          try {
            const res = await messageApi.list(channelId)
            if (generation !== this.lifecycleGeneration) return
            useMessageStore
              .getState()
              .setMessages(channelId, res.data ?? [], res.meta?.cursor ?? '', res.meta?.has_more ?? false)
          } catch {
            if (generation !== this.lifecycleGeneration) return
            /* one channel failing must not abort the rest */
          }
        }),
      )
      if (generation !== this.lifecycleGeneration) return

      try {
        const res = await notificationApi.unreadCount()
        if (generation !== this.lifecycleGeneration) return
        useUiStore.getState().setUnread(res.data?.count ?? 0)
      } catch {
        if (generation !== this.lifecycleGeneration) return
        /* badge stays stale */
      }
    } finally {
      if (generation === this.lifecycleGeneration) this.resyncInFlight = false
    }

    if (generation !== this.lifecycleGeneration) return
    this.resyncListeners.forEach((l) => l(reason))
  }

  // ---------------------------------------------------------------------------
  // Sending
  // ---------------------------------------------------------------------------

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

  // ---------------------------------------------------------------------------
  // Collaboration rooms
  // ---------------------------------------------------------------------------

  joinRoom(documentId: string) {
    if (this.desiredRooms.has(documentId)) return
    this.desiredRooms.add(documentId)
    this.send('collab.join', { document_id: documentId })
  }

  leaveRoom(documentId: string) {
    if (!this.desiredRooms.delete(documentId)) return
    this.joinedRooms.delete(documentId)
    this.send('collab.leave', { document_id: documentId })
  }

  /** What the server said this connection may do in a room, or undefined. */
  roomAccess(documentId: string) {
    return this.joinedRooms.get(documentId)
  }

  sendCollabUpdate(documentId: string, updateBase64: string): boolean {
    return this.send('collab.update', { document_id: documentId, update: updateBase64 })
  }

  sendAwareness(documentId: string, stateBase64: string) {
    this.send('collab.awareness', { document_id: documentId, state: stateBase64 })
  }

  onHuddle(listener: (e: HuddleEvent) => void) {
    this.huddleListeners.add(listener)
    return () => this.huddleListeners.delete(listener)
  }

  private emitHuddle(e: HuddleEvent) {
    this.huddleListeners.forEach((l) => l(e))
  }

  onRoom(listener: (e: RoomEvent) => void) {
    this.roomListeners.add(listener)
    return () => this.roomListeners.delete(listener)
  }

  private emitRoom(e: RoomEvent) {
    this.roomListeners.forEach((l) => l(e))
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
        const generation = this.lifecycleGeneration
        this.typingTimers.set(
          key,
          setTimeout(() => {
            if (generation !== this.lifecycleGeneration) return
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
      // --- huddles -----------------------------------------------------------
      case 'huddle.started': {
        const channelId = str(d, 'channel_id')
        const huddleId = str(d, 'huddle_id')
        if (!channelId || !huddleId) break
        this.emitHuddle({
          kind: 'started',
          channelId,
          huddleId,
          startedBy: str(d, 'started_by') ?? '',
        })
        break
      }
      case 'huddle.ended': {
        const channelId = str(d, 'channel_id')
        const huddleId = str(d, 'huddle_id')
        if (!channelId || !huddleId) break
        this.emitHuddle({ kind: 'ended', channelId, huddleId, reason: str(d, 'reason') ?? '' })
        break
      }
      case 'huddle.roster': {
        const channelId = str(d, 'channel_id')
        const huddleId = str(d, 'huddle_id')
        if (!channelId || !huddleId) break
        // The frame is a NUDGE, not a payload: it says the roster changed and
        // the reader refetches. Sending the roster itself would mean deciding
        // per subscriber what they may see, which is what the pull path
        // already does correctly.
        this.emitHuddle({ kind: 'roster', channelId, huddleId })
        break
      }

      // --- collaboration -----------------------------------------------------
      //
      // These arms keep no document state. The CRDT lives in the provider and
      // the manager is a transport: it tracks which rooms this connection is in
      // (so a reconnect can restore them) and forwards everything else.
      case 'collab.joined': {
        const documentId = str(d, 'document_id')
        if (!documentId) break
        const headSeq = num(d, 'head_seq') ?? 0
        const canWrite = d?.can_write === true
        this.joinedRooms.set(documentId, { canWrite, headSeq })
        this.emitRoom({ kind: 'joined', documentId, headSeq, canWrite })
        break
      }
      case 'collab.left': {
        const documentId = str(d, 'document_id')
        if (!documentId) break
        this.joinedRooms.delete(documentId)
        const revoked = str(d, 'reason') === 'revoked'
        if (revoked) {
          // Access was withdrawn. Drop the DESIRE too, or the next reconnect
          // re-joins a room the server will refuse — and the editor would show
          // a document nobody may open, forever retrying.
          this.desiredRooms.delete(documentId)
        }
        this.emitRoom({ kind: 'left', documentId, reason: revoked ? 'revoked' : 'client' })
        break
      }
      case 'collab.update': {
        const documentId = str(d, 'document_id')
        const update = str(d, 'update')
        if (!documentId || !update) break
        this.emitRoom({
          kind: 'update',
          documentId,
          seq: num(d, 'seq') ?? 0,
          actorId: str(d, 'actor_id') ?? '',
          originConn: str(d, 'origin_conn') ?? '',
          update,
        })
        break
      }
      case 'collab.awareness': {
        const documentId = str(d, 'document_id')
        const state = str(d, 'state')
        if (!documentId || !state) break
        this.emitRoom({
          kind: 'awareness',
          documentId,
          actorId: str(d, 'actor_id') ?? '',
          originConn: str(d, 'origin_conn') ?? '',
          state,
        })
        break
      }
      case 'collab.compact': {
        const documentId = str(d, 'document_id')
        if (!documentId) break
        this.emitRoom({
          kind: 'compact',
          documentId,
          headSeq: num(d, 'head_seq') ?? 0,
          snapshotSeq: num(d, 'snapshot_seq') ?? 0,
        })
        break
      }
      case 'collab.project': {
        const documentId = str(d, 'document_id')
        if (!documentId) break
        this.emitRoom({
          kind: 'project',
          documentId,
          headSeq: num(d, 'head_seq') ?? 0,
          projectionSeq: num(d, 'projection_seq') ?? 0,
        })
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
