import { useState, useEffect, useRef } from 'react'
import { notificationApi, type Notification } from '@/api/notifications'
import Badge from './Badge'

export default function NotificationBell() {
  const [open, setOpen] = useState(false)
  const [notifications, setNotifications] = useState<Notification[]>([])
  const [unreadCount, setUnreadCount] = useState(0)
  const ref = useRef<HTMLDivElement>(null)

  useEffect(() => {
    notificationApi.unreadCount().then((r) => setUnreadCount(r.data.count)).catch(() => {})
    const interval = setInterval(() => {
      notificationApi.unreadCount().then((r) => setUnreadCount(r.data.count)).catch(() => {})
    }, 30000)
    return () => clearInterval(interval)
  }, [])

  useEffect(() => {
    if (open) {
      notificationApi.list().then((r) => setNotifications(r.data)).catch(() => {})
    }
  }, [open])

  useEffect(() => {
    const handler = (e: MouseEvent) => {
      if (ref.current && !ref.current.contains(e.target as Node)) setOpen(false)
    }
    document.addEventListener('mousedown', handler)
    return () => document.removeEventListener('mousedown', handler)
  }, [])

  const handleMarkAllRead = async () => {
    await notificationApi.markAllRead().catch(() => {})
    setUnreadCount(0)
    setNotifications((n) => n.map((x) => ({ ...x, is_read: true })))
  }

  return (
    <div ref={ref} className="relative">
      <button onClick={() => setOpen(!open)} className="relative text-surface-200 hover:text-white p-1.5 rounded-lg hover:bg-surface-800 transition-colors">
        <svg className="w-5 h-5" fill="none" stroke="currentColor" viewBox="0 0 24 24">
          <path strokeLinecap="round" strokeLinejoin="round" strokeWidth={2} d="M15 17h5l-1.405-1.405A2.032 2.032 0 0118 14.158V11a6.002 6.002 0 00-4-5.659V5a2 2 0 10-4 0v.341C7.67 6.165 6 8.388 6 11v3.159c0 .538-.214 1.055-.595 1.436L4 17h5m6 0v1a3 3 0 11-6 0v-1m6 0H9" />
        </svg>
        {unreadCount > 0 && (
          <span className="absolute -top-1 -right-1"><Badge count={unreadCount} /></span>
        )}
      </button>

      {open && (
        <div className="absolute right-0 top-full mt-2 w-80 bg-surface-900 border border-surface-700/50 rounded-xl shadow-2xl z-50 overflow-hidden">
          <div className="flex items-center justify-between px-4 py-3 border-b border-surface-700/50">
            <span className="text-sm font-semibold text-white">Notifications</span>
            {unreadCount > 0 && (
              <button onClick={handleMarkAllRead} className="text-xs text-brand-400 hover:text-brand-300">
                Mark all read
              </button>
            )}
          </div>
          <div className="max-h-80 overflow-y-auto">
            {notifications.length === 0 ? (
              <div className="py-8 text-center text-surface-200/50 text-sm">No notifications</div>
            ) : (
              notifications.map((n) => (
                <div key={n.id} className={`px-4 py-3 border-b border-surface-700/30 ${!n.is_read ? 'bg-brand-600/5' : ''}`}>
                  <div className="text-sm text-white">{n.title}</div>
                  <div className="text-xs text-surface-200/60 mt-0.5">{n.body}</div>
                  <div className="text-xs text-surface-200/40 mt-1">{new Date(n.created_at).toLocaleString()}</div>
                </div>
              ))
            )}
          </div>
        </div>
      )}
    </div>
  )
}
