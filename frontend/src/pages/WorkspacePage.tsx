import { useEffect, useState, useCallback } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { channelApi } from '@/api/channels'
import { workspaceApi } from '@/api/workspaces'
import { useWorkspaceStore } from '@/stores/workspaceStore'
import { useChannelStore } from '@/stores/channelStore'
import { useThreadStore } from '@/stores/threadStore'
import { wsManager } from '@/lib/websocket'
import Sidebar from '@/components/layout/Sidebar'
import ChannelView from '@/components/channel/ChannelView'
import ThreadPanel from '@/components/thread/ThreadPanel'
import SearchModal from '@/components/search/SearchModal'
import AdminPage from '@/pages/AdminPage'

export default function WorkspacePage() {
  const { workspaceId, '*': splat } = useParams()
  const navigate = useNavigate()
  const { setActiveWorkspace } = useWorkspaceStore()
  const { setChannels, channels } = useChannelStore()
  const activeThreadId = useThreadStore((s) => s.activeThreadId)
  const [searchOpen, setSearchOpen] = useState(false)
  const [sidebarOpen, setSidebarOpen] = useState(true)

  const channelId = splat?.startsWith('c/') ? splat.slice(2) : undefined
  const isAdmin = splat === 'admin'

  useEffect(() => {
    if (!workspaceId) return
    workspaceApi.get(workspaceId).then((res) => setActiveWorkspace(res.data)).catch(() => navigate('/setup'))
    channelApi.list(workspaceId).then((res) => setChannels(res.data)).catch(() => {})
    wsManager.connect()
    return () => wsManager.disconnect()
  }, [workspaceId, setActiveWorkspace, setChannels, navigate])

  useEffect(() => {
    channels.forEach((ch) => wsManager.subscribe(ch.id))
    return () => channels.forEach((ch) => wsManager.unsubscribe(ch.id))
  }, [channels])

  // Cmd+K search shortcut
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); setSearchOpen((o) => !o) }
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [])

  return (
    <div className="h-screen flex overflow-hidden bg-surface-950">
      {/* Mobile overlay */}
      {sidebarOpen && (
        <div className="md:hidden fixed inset-0 bg-black/50 z-30" onClick={() => setSidebarOpen(false)} />
      )}
      <div className={`${sidebarOpen ? 'translate-x-0' : '-translate-x-full'} md:translate-x-0 fixed md:static z-40 md:z-auto transition-transform duration-200`}>
        <Sidebar onNavigate={() => setSidebarOpen(false)} />
      </div>

      <main className="flex-1 flex min-w-0">
        <div className="flex-1 flex flex-col min-w-0">
          {/* Mobile header */}
          <div className="md:hidden h-14 px-4 flex items-center border-b border-surface-700/50 bg-surface-950">
            <button onClick={() => setSidebarOpen(true)} className="text-surface-200 hover:text-white mr-3 text-xl">&#9776;</button>
            <span className="font-semibold text-white">SuperOps</span>
          </div>

          {isAdmin ? (
            <AdminPage />
          ) : channelId ? (
            <ChannelView key={channelId} channelId={channelId} />
          ) : (
            <div className="flex-1 flex items-center justify-center text-surface-200">
              <div className="text-center">
                <div className="text-4xl mb-4">💬</div>
                <p className="text-lg font-medium">Select a channel to start chatting</p>
                <p className="text-sm mt-1 text-surface-200/60">Press <kbd className="px-1.5 py-0.5 bg-surface-800 rounded text-xs">⌘K</kbd> to search</p>
              </div>
            </div>
          )}
        </div>

        {/* Thread panel */}
        {activeThreadId && <ThreadPanel />}
      </main>

      <SearchModal open={searchOpen} onClose={() => setSearchOpen(false)} />
    </div>
  )
}
