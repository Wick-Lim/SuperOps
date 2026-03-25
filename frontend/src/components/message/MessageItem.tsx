import type { Message } from '@/lib/types'
import { useThreadStore } from '@/stores/threadStore'

interface Props {
  message: Message
  showHeader: boolean
}

function formatTime(dateStr: string) {
  const d = new Date(dateStr)
  return d.toLocaleTimeString([], { hour: '2-digit', minute: '2-digit' })
}

function formatDate(dateStr: string) {
  const d = new Date(dateStr)
  const today = new Date()
  if (d.toDateString() === today.toDateString()) return 'Today'
  const yesterday = new Date(today)
  yesterday.setDate(yesterday.getDate() - 1)
  if (d.toDateString() === yesterday.toDateString()) return 'Yesterday'
  return d.toLocaleDateString([], { month: 'short', day: 'numeric' })
}

export default function MessageItem({ message, showHeader }: Props) {
  const initial = message.user_id.slice(0, 2).toUpperCase()
  const color = `hsl(${message.user_id.split('').reduce((a, c) => a + c.charCodeAt(0), 0) % 360}, 60%, 50%)`

  return (
    <div className={`group flex gap-3 ${showHeader ? 'mt-4 first:mt-0' : 'mt-0.5'} hover:bg-surface-900/50 -mx-2 px-2 py-0.5 rounded`}>
      {showHeader ? (
        <div
          className="w-9 h-9 rounded-lg flex items-center justify-center text-white text-xs font-medium shrink-0 mt-0.5"
          style={{ backgroundColor: color }}
        >
          {initial}
        </div>
      ) : (
        <div className="w-9 shrink-0 flex items-center justify-center">
          <span className="text-xs text-surface-200/0 group-hover:text-surface-200/50 transition-colors">
            {formatTime(message.created_at)}
          </span>
        </div>
      )}

      <div className="min-w-0 flex-1">
        {showHeader && (
          <div className="flex items-baseline gap-2 mb-0.5">
            <span className="font-semibold text-sm text-white">{message.user_id.slice(0, 8)}</span>
            <span className="text-xs text-surface-200/50">
              {formatDate(message.created_at)} {formatTime(message.created_at)}
            </span>
            {message.is_edited && (
              <span className="text-xs text-surface-200/40">(edited)</span>
            )}
          </div>
        )}
        <div className="text-sm text-surface-100 whitespace-pre-wrap break-words leading-relaxed">
          {message.content}
        </div>
        {message.reply_count > 0 && (
          <button onClick={() => useThreadStore.getState().openThread(message)}
            className="text-xs text-brand-400 hover:text-brand-300 mt-1">
            {message.reply_count} {message.reply_count === 1 ? 'reply' : 'replies'}
          </button>
        )}
        {!message.parent_id && message.reply_count === 0 && (
          <button onClick={() => useThreadStore.getState().openThread(message)}
            className="text-xs text-surface-200/0 group-hover:text-surface-200/40 hover:!text-brand-400 mt-1 transition-colors">
            Reply
          </button>
        )}
      </div>
    </div>
  )
}
