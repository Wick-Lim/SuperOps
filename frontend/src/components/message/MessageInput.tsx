import { useState, useRef, useEffect } from 'react'
import { wsManager } from '@/lib/websocket'
import FileUpload from '@/components/file/FileUpload'

interface Props {
  onSend: (content: string) => void
  channelName: string
  channelId?: string
}

export default function MessageInput({ onSend, channelName, channelId }: Props) {
  const [content, setContent] = useState('')
  const textareaRef = useRef<HTMLTextAreaElement>(null)
  const typingTimer = useRef<ReturnType<typeof setTimeout> | null>(null)

  useEffect(() => {
    textareaRef.current?.focus()
  }, [channelName])

  const handleKeyDown = (e: React.KeyboardEvent) => {
    if (e.key === 'Enter' && !e.shiftKey) {
      e.preventDefault()
      if (content.trim()) {
        onSend(content.trim())
        setContent('')
      }
    }
  }

  // Auto-resize textarea
  useEffect(() => {
    const el = textareaRef.current
    if (el) {
      el.style.height = 'auto'
      el.style.height = Math.min(el.scrollHeight, 160) + 'px'
    }
  }, [content])

  return (
    <div className="px-5 pb-5 pt-2 shrink-0">
      <div className="bg-surface-900 border border-surface-700/50 rounded-xl overflow-hidden focus-within:ring-1 focus-within:ring-brand-500/50 focus-within:border-brand-500/50 transition-all">
        <textarea
          ref={textareaRef}
          value={content}
          onChange={(e) => {
            setContent(e.target.value)
            if (channelId && e.target.value) {
              if (!typingTimer.current) {
                wsManager.sendTyping(channelId)
              }
              if (typingTimer.current) clearTimeout(typingTimer.current)
              typingTimer.current = setTimeout(() => { typingTimer.current = null }, 2000)
            }
          }}
          onKeyDown={handleKeyDown}
          placeholder={`Message #${channelName}`}
          rows={1}
          className="w-full px-4 py-3 bg-transparent text-white placeholder-surface-200/40 resize-none focus:outline-none text-sm leading-relaxed"
        />
        <div className="flex items-center justify-between px-3 py-2 border-t border-surface-700/30">
          <div className="flex gap-1 items-center">
            <FileUpload onUploaded={(file) => {
              onSend(`[file: ${file.name}](/api/v1/files/${file.id})`)
            }} />
          </div>
          <button
            onClick={() => { if (content.trim()) { onSend(content.trim()); setContent('') } }}
            disabled={!content.trim()}
            className="px-3 py-1.5 bg-brand-600 hover:bg-brand-700 disabled:opacity-30 disabled:cursor-not-allowed rounded-lg text-white text-sm font-medium transition-colors"
          >
            Send
          </button>
        </div>
      </div>
    </div>
  )
}
