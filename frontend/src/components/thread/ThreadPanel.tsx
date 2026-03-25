import { useEffect, useState } from 'react'
import { messageApi } from '@/api/messages'
import { useThreadStore } from '@/stores/threadStore'
import MessageItem from '@/components/message/MessageItem'

export default function ThreadPanel() {
  const { activeThreadId, parentMessage, replies, setReplies, addReply, closeThread } = useThreadStore()
  const [content, setContent] = useState('')

  useEffect(() => {
    if (!activeThreadId) return
    messageApi.listThread(activeThreadId).then((res) => {
      setReplies(res.data)
    }).catch(() => {})
  }, [activeThreadId, setReplies])

  if (!activeThreadId || !parentMessage) return null

  const handleSend = async () => {
    if (!content.trim()) return
    try {
      const res = await messageApi.replyThread(activeThreadId, content.trim())
      addReply(res.data)
      setContent('')
    } catch { /* ignore */ }
  }

  return (
    <div className="w-96 border-l border-surface-700/50 flex flex-col bg-surface-950 shrink-0">
      <div className="h-14 px-4 flex items-center justify-between border-b border-surface-700/50">
        <h3 className="font-semibold text-white text-sm">Thread</h3>
        <button onClick={closeThread} className="text-surface-200 hover:text-white text-lg">&times;</button>
      </div>

      <div className="flex-1 overflow-y-auto p-4 space-y-3">
        <div className="pb-3 border-b border-surface-700/30">
          <MessageItem message={parentMessage} showHeader />
        </div>
        <div className="text-xs text-surface-200/50 py-1">
          {replies.length} {replies.length === 1 ? 'reply' : 'replies'}
        </div>
        {replies.map((r) => (
          <MessageItem key={r.id} message={r} showHeader />
        ))}
      </div>

      <div className="p-3 border-t border-surface-700/50">
        <div className="flex gap-2">
          <input
            type="text"
            value={content}
            onChange={(e) => setContent(e.target.value)}
            onKeyDown={(e) => { if (e.key === 'Enter' && !e.shiftKey) { e.preventDefault(); handleSend() } }}
            placeholder="Reply..."
            className="flex-1 px-3 py-2 bg-surface-900 border border-surface-700/50 rounded-lg text-sm text-white placeholder-surface-200/40 focus:outline-none focus:ring-1 focus:ring-brand-500"
          />
          <button onClick={handleSend} disabled={!content.trim()}
            className="px-3 py-2 bg-brand-600 hover:bg-brand-700 disabled:opacity-30 rounded-lg text-white text-sm font-medium">
            Reply
          </button>
        </div>
      </div>
    </div>
  )
}
