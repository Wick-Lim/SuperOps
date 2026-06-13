import { useAuthStore } from '../stores/authStore'
import { useMessageStore } from '../stores/messageStore'
import { useUiStore } from '../stores/uiStore'
import { WS_BASE_URL } from '../config'
import type { Message, Reaction, PresenceStatus } from './types'

type WSEventHandler = (data: unknown) => void

class WebSocketManager {
  private ws: WebSocket | null = null
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectDelay = 1000
  private connected = false
  private handlers: Map<string, WSEventHandler[]> = new Map()
  private subscribedChannels: Set<string> = new Set()
  private typingTimers: Map<string, ReturnType<typeof setTimeout>> = new Map()
  private statusListeners: Set<(connected: boolean) => void> = new Set()

  connect() {
    const token = useAuthStore.getState().accessToken
    if (!token) return
    if (this.ws && (this.ws.readyState === WebSocket.OPEN || this.ws.readyState === WebSocket.CONNECTING)) return

    const url = `${WS_BASE_URL}?token=${token}`
    this.ws = new WebSocket(url)

    this.ws.onopen = () => {
      this.reconnectDelay = 1000
      this.setConnected(true)
      this.subscribedChannels.forEach((chId) => this.send('subscribe', { channel_id: chId }))
    }

    this.ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data)
        this.dispatch(msg.type, msg.data)
      } catch {}
    }

    this.ws.onclose = () => { this.setConnected(false); this.scheduleReconnect() }
    this.ws.onerror = () => this.ws?.close()
  }

  disconnect() {
    if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null }
    this.ws?.close()
    this.ws = null
    this.setConnected(false)
  }

  isConnected() { return this.connected }

  onStatus(cb: (connected: boolean) => void) {
    this.statusListeners.add(cb)
    return () => this.statusListeners.delete(cb)
  }

  private setConnected(v: boolean) {
    this.connected = v
    this.statusListeners.forEach((l) => l(v))
  }

  private scheduleReconnect() {
    if (this.reconnectTimer) return
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, 30000)
      this.connect()
    }, this.reconnectDelay)
  }

  send(type: string, data: unknown) {
    if (this.ws?.readyState === WebSocket.OPEN) {
      this.ws.send(JSON.stringify({ type, data }))
    }
  }

  subscribe(channelId: string) {
    this.subscribedChannels.add(channelId)
    this.send('subscribe', { channel_id: channelId })
  }

  unsubscribe(channelId: string) {
    this.subscribedChannels.delete(channelId)
    this.send('unsubscribe', { channel_id: channelId })
  }

  sendTyping(channelId: string) {
    this.send('typing.start', { channel_id: channelId })
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

  private dispatch(type: string, data: unknown) {
    const ms = useMessageStore.getState()
    const ui = useUiStore.getState()

    switch (type) {
      case 'message.new': {
        const m = data as Message
        ms.addMessage(m.channel_id, m)
        break
      }
      case 'message.updated': {
        const m = data as Message
        ms.updateMessage(m.channel_id, m)
        break
      }
      case 'message.deleted': {
        const d = data as { id: string; channel_id: string }
        ms.removeMessage(d.channel_id, d.id)
        break
      }
      case 'reaction.added':
      case 'reaction.removed': {
        const d = data as { channel_id: string; message_id: string; user_id: string; emoji: string }
        const r: Reaction = { id: '', message_id: d.message_id, user_id: d.user_id, emoji: d.emoji, created_at: '' }
        ms.applyReaction(d.channel_id, r, type === 'reaction.added')
        break
      }
      case 'typing.indicator': {
        const d = data as { channel_id: string; user_id: string }
        if (d.user_id === useAuthStore.getState().user?.id) break
        ui.addTyping(d.channel_id, d.user_id)
        const key = `${d.channel_id}:${d.user_id}`
        const prev = this.typingTimers.get(key)
        if (prev) clearTimeout(prev)
        this.typingTimers.set(key, setTimeout(() => {
          useUiStore.getState().removeTyping(d.channel_id, d.user_id)
          this.typingTimers.delete(key)
        }, 4000))
        break
      }
      case 'presence.changed': {
        const d = data as { user_id: string; status: PresenceStatus }
        ui.setUserPresence(d.user_id, d.status)
        break
      }
      case 'notification.new': {
        ui.setUnread(useUiStore.getState().unreadNotifications + 1)
        break
      }
    }

    const handlers = this.handlers.get(type) || []
    handlers.forEach((h) => h(data))
  }
}

export const wsManager = new WebSocketManager()
