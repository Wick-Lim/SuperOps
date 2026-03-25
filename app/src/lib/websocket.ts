import { useAuthStore } from '../stores/authStore'
import { useMessageStore } from '../stores/messageStore'
import { WS_BASE_URL } from '../config'
import type { Message } from './types'

type WSEventHandler = (data: unknown) => void

class WebSocketManager {
  private ws: WebSocket | null = null
  private reconnectTimer: ReturnType<typeof setTimeout> | null = null
  private reconnectDelay = 1000
  private handlers: Map<string, WSEventHandler[]> = new Map()
  private subscribedChannels: Set<string> = new Set()

  connect() {
    const token = useAuthStore.getState().accessToken
    if (!token) return

    const url = `${WS_BASE_URL}?token=${token}`
    this.ws = new WebSocket(url)

    this.ws.onopen = () => {
      this.reconnectDelay = 1000
      this.subscribedChannels.forEach((chId) => this.send('subscribe', { channel_id: chId }))
    }

    this.ws.onmessage = (event) => {
      try {
        const msg = JSON.parse(event.data)
        this.dispatch(msg.type, msg.data)
      } catch {}
    }

    this.ws.onclose = () => this.scheduleReconnect()
    this.ws.onerror = () => this.ws?.close()
  }

  disconnect() {
    if (this.reconnectTimer) { clearTimeout(this.reconnectTimer); this.reconnectTimer = null }
    this.ws?.close()
    this.ws = null
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

  on(type: string, handler: WSEventHandler) {
    const handlers = this.handlers.get(type) || []
    handlers.push(handler)
    this.handlers.set(type, handlers)
  }

  private dispatch(type: string, data: unknown) {
    if (type === 'message.new') {
      const msg = data as Message
      useMessageStore.getState().addMessage(msg.channel_id, msg)
    }

    const handlers = this.handlers.get(type) || []
    handlers.forEach((h) => h(data))
  }
}

export const wsManager = new WebSocketManager()
