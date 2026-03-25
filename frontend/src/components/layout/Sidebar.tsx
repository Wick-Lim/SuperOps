import { useState } from 'react'
import { useNavigate, useParams } from 'react-router-dom'
import { useAuthStore } from '@/stores/authStore'
import { useWorkspaceStore } from '@/stores/workspaceStore'
import { useChannelStore } from '@/stores/channelStore'
import { channelApi } from '@/api/channels'

export default function Sidebar() {
  const { workspaceId, '*': splat } = useParams()
  const channelId = splat?.startsWith('c/') ? splat.slice(2) : undefined
  const navigate = useNavigate()
  const user = useAuthStore((s) => s.user)
  const logout = useAuthStore((s) => s.logout)
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const { channels, setChannels, setActiveChannel } = useChannelStore()
  const [showCreate, setShowCreate] = useState(false)
  const [newName, setNewName] = useState('')

  const handleCreateChannel = async (e: React.FormEvent) => {
    e.preventDefault()
    if (!workspaceId || !newName.trim()) return

    try {
      const slug = newName.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')
      const res = await channelApi.create(workspaceId, { name: newName, slug })
      setChannels([...channels, res.data])
      setShowCreate(false)
      setNewName('')
      navigate(`/w/${workspaceId}/c/${res.data.id}`)
    } catch {
      // ignore
    }
  }

  const selectChannel = (ch: typeof channels[0]) => {
    setActiveChannel(ch)
    navigate(`/w/${workspaceId}/c/${ch.id}`)
  }

  return (
    <aside className="w-64 bg-surface-900 border-r border-surface-700/50 flex flex-col shrink-0">
      {/* Workspace header */}
      <div className="h-14 px-4 flex items-center border-b border-surface-700/50">
        <div className="w-8 h-8 bg-brand-600 rounded-lg flex items-center justify-center text-white font-bold text-sm mr-3 shrink-0">
          {workspace?.name?.[0]?.toUpperCase() || 'S'}
        </div>
        <span className="font-semibold text-white truncate">{workspace?.name || 'SuperOps'}</span>
      </div>

      {/* Channels */}
      <div className="flex-1 overflow-y-auto py-3">
        <div className="px-3 mb-1 flex items-center justify-between">
          <span className="text-xs font-semibold text-surface-200 uppercase tracking-wider">Channels</span>
          <button
            onClick={() => setShowCreate(!showCreate)}
            className="w-5 h-5 flex items-center justify-center text-surface-200 hover:text-white hover:bg-surface-700 rounded transition-colors text-lg leading-none"
          >
            +
          </button>
        </div>

        {showCreate && (
          <form onSubmit={handleCreateChannel} className="px-3 mb-2">
            <input
              type="text"
              value={newName}
              onChange={(e) => setNewName(e.target.value)}
              placeholder="channel-name"
              autoFocus
              className="w-full px-2 py-1.5 bg-surface-800 border border-surface-700 rounded text-sm text-white placeholder-surface-200/50 focus:outline-none focus:ring-1 focus:ring-brand-500"
              onBlur={() => { if (!newName) setShowCreate(false) }}
            />
          </form>
        )}

        <div className="space-y-0.5 px-1.5">
          {channels.map((ch) => (
            <button
              key={ch.id}
              onClick={() => selectChannel(ch)}
              className={`w-full text-left px-2.5 py-1.5 rounded-md text-sm flex items-center transition-colors ${
                ch.id === channelId
                  ? 'bg-brand-600/20 text-brand-300'
                  : 'text-surface-200 hover:bg-surface-800 hover:text-white'
              }`}
            >
              <span className="mr-2 text-surface-200/60">#</span>
              <span className="truncate">{ch.name || 'unnamed'}</span>
            </button>
          ))}
        </div>
      </div>

      {/* User footer */}
      <div className="h-14 px-3 flex items-center border-t border-surface-700/50">
        <div className="w-8 h-8 bg-green-600 rounded-full flex items-center justify-center text-white font-medium text-xs mr-2 shrink-0">
          {user?.full_name?.[0]?.toUpperCase() || user?.username?.[0]?.toUpperCase() || '?'}
        </div>
        <div className="flex-1 min-w-0">
          <div className="text-sm font-medium text-white truncate">{user?.full_name || user?.username}</div>
        </div>
        <button
          onClick={() => { logout(); navigate('/login') }}
          className="text-surface-200 hover:text-white text-xs ml-2"
          title="Sign out"
        >
          &#x2190;
        </button>
      </div>
    </aside>
  )
}
