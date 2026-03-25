import { useState } from 'react'
import { useNavigate } from 'react-router-dom'
import Modal from '@/components/shared/Modal'
import { api } from '@/api/client'
import { useWorkspaceStore } from '@/stores/workspaceStore'
import { useChannelStore } from '@/stores/channelStore'
import type { Channel } from '@/lib/types'

interface Props {
  open: boolean
  onClose: () => void
}

export default function DMCreate({ open, onClose }: Props) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<Array<{ id: string; username: string; full_name: string }>>([])
  const [loading, setLoading] = useState(false)
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const addChannel = useChannelStore((s) => s.addChannel)
  const navigate = useNavigate()

  const handleSearch = async (q: string) => {
    setQuery(q)
    if (q.length < 2) { setResults([]); return }
    setLoading(true)
    try {
      const res = await api.get<Array<{ id: string; username: string; full_name: string }>>(`/users/search?q=${q}`)
      setResults(res.data)
    } catch { setResults([]) }
    finally { setLoading(false) }
  }

  const handleSelect = async (userId: string) => {
    if (!workspace) return
    try {
      const res = await api.post<Channel>(`/workspaces/${workspace.id}/dm`, { user_ids: [userId] })
      addChannel(res.data)
      onClose()
      navigate(`/w/${workspace.id}/c/${res.data.id}`)
    } catch { /* ignore */ }
  }

  return (
    <Modal open={open} onClose={onClose} title="New Direct Message">
      <input
        type="text" value={query} onChange={(e) => handleSearch(e.target.value)}
        placeholder="Search users..."
        autoFocus
        className="w-full px-3 py-2 bg-surface-800 border border-surface-700 rounded-lg text-sm text-white placeholder-surface-200/40 focus:outline-none focus:ring-1 focus:ring-brand-500"
      />
      <div className="mt-3 max-h-60 overflow-y-auto">
        {loading && <div className="py-4 text-center text-surface-200/50 text-sm">Searching...</div>}
        {results.map((u) => (
          <button key={u.id} onClick={() => handleSelect(u.id)}
            className="w-full flex items-center gap-3 px-3 py-2.5 rounded-lg hover:bg-surface-800 transition-colors text-left">
            <div className="w-8 h-8 rounded-lg bg-brand-600 flex items-center justify-center text-white text-xs font-medium">
              {u.full_name?.[0]?.toUpperCase() || u.username[0].toUpperCase()}
            </div>
            <div>
              <div className="text-sm text-white font-medium">{u.full_name || u.username}</div>
              <div className="text-xs text-surface-200/50">@{u.username}</div>
            </div>
          </button>
        ))}
        {query.length >= 2 && !loading && results.length === 0 && (
          <div className="py-4 text-center text-surface-200/50 text-sm">No users found</div>
        )}
      </div>
    </Modal>
  )
}
