import { useAuthStore } from '@/stores/authStore'
import { useMessageStore } from '@/stores/messageStore'
import { usePresenceStore } from '@/stores/presenceStore'

type WSEventHandler = (data: unknown) => void

class WebSocketManager {
  private ws: WebSocket | null = null
  private url: string = ''
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectDelay = 1000
  private maxReconnectDelay = 30000
  private handlers: Map<string, WSEventHandler[]> = new Map()
  private subscribedChannels: Set<string> = new Set()

  connect() {
    const token = useAuthStore.getState().accessToken
    if (!token) return

    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
    this.url = `${protocol}//${window.location.host}/api/v1/ws?token=${token}`

    this.ws = new WebSocket(this.url)

    this.ws.onopen = () => {
      console.log('[WS] connected')
      this.reconnectDelay = 1000
      // Re-subscribe to channels
      this.subscribedChannels.forEach((chId) => {
        this.send('subscribe', { channel_id: chId })
      })
    }

    this.ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data)
        this.dispatch(msg.type, msg.data)
      } catch {
        // ignore parse errors
      }
    }

    this.ws.onclose = () => {
      console.log('[WS] disconnected, reconnecting...')
      this.scheduleReconnect()
    }

    this.ws.onerror = () => {
      this.ws?.close()
    }
  }

  disconnect() {
    if (this.reconnectTimer) {
      clearTimeout(this.reconnectTimer)
      this.reconnectTimer = null
    }
    this.ws?.close()
    this.ws = null
  }

  private scheduleReconnect() {
    if (this.reconnectTimer) return
    this.reconnectTimer = setTimeout(() => {
      this.reconnectTimer = null
      this.reconnectDelay = Math.min(this.reconnectDelay * 2, this.maxReconnectDelay)
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

  on(type: string, handler: WSEventHandler) {
    const handlers = this.handlers.get(type) || []
    handlers.push(handler)
    this.handlers.set(type, handlers)
  }

  off(type: string, handler: WSEventHandler) {
    const handlers = this.handlers.get(type) || []
    this.handlers.set(type, handlers.filter((h) => h !== handler))
  }

  sendTyping(channelId: string) {
    this.send('typing.start', { channel_id: channelId })
  }

  private dispatch(type: string, data: unknown) {
    // Built-in handlers
    if (type === 'message.new') {
      const msg = data as { channel_id: string }
      useMessageStore.getState().addMessage(msg.channel_id, data as import('@/lib/types').Message)
    }

    if (type === 'presence.changed') {
      const d = data as { user_id: string; status: string }
      usePresenceStore.getState().setPresence(d.user_id, d.status)
    }

    if (type === 'typing.indicator') {
      const d = data as { channel_id: string; user_id: string }
      usePresenceStore.getState().setTyping(d.channel_id, d.user_id)
      // Auto-clear after 3 seconds
      setTimeout(() => {
        usePresenceStore.getState().clearTyping(d.channel_id, d.user_id)
      }, 3000)
    }

    // Custom handlers
    const handlers = this.handlers.get(type) || []
    handlers.forEach((h) => h(data))
  }
}

export const wsManager = new WebSocketManager()
