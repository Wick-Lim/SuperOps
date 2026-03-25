import { useEffect } from 'react'
import { useParams, useNavigate } from 'react-router-dom'
import { channelApi } from '@/api/channels'
import { workspaceApi } from '@/api/workspaces'
import { useWorkspaceStore } from '@/stores/workspaceStore'
import { useChannelStore } from '@/stores/channelStore'
import { wsManager } from '@/lib/websocket'
import Sidebar from '@/components/layout/Sidebar'
import ChannelView from '@/components/channel/ChannelView'

export default function WorkspacePage() {
  const { workspaceId, '*': splat } = useParams()
  const navigate = useNavigate()
  const { setActiveWorkspace } = useWorkspaceStore()
  const { setChannels, channels } = useChannelStore()

  // Extract channelId from splat: "c/<uuid>"
  const channelId = splat?.startsWith('c/') ? splat.slice(2) : undefined

  useEffect(() => {
    if (!workspaceId) return

    workspaceApi.get(workspaceId).then((res) => {
      setActiveWorkspace(res.data)
    }).catch(() => navigate('/setup'))

    channelApi.list(workspaceId).then((res) => {
      setChannels(res.data)
    }).catch(() => {})

    wsManager.connect()
    return () => wsManager.disconnect()
  }, [workspaceId, setActiveWorkspace, setChannels, navigate])

  useEffect(() => {
    channels.forEach((ch) => wsManager.subscribe(ch.id))
    return () => channels.forEach((ch) => wsManager.unsubscribe(ch.id))
  }, [channels])

  return (
    <div className="h-screen flex overflow-hidden bg-surface-950">
      <Sidebar />
      <main className="flex-1 flex flex-col min-w-0">
        {channelId ? (
          <ChannelView key={channelId} channelId={channelId} />
        ) : (
          <div className="flex-1 flex items-center justify-center text-surface-200">
            <div className="text-center">
              <div className="text-4xl mb-4">💬</div>
              <p className="text-lg font-medium">Select a channel to start chatting</p>
              <p className="text-sm mt-1 text-surface-200/60">Or create a new channel from the sidebar</p>
            </div>
          </div>
        )}
      </main>
    </div>
  )
}
