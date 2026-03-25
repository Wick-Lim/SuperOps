import { useState, useEffect } from 'react'
import { useNavigate } from 'react-router-dom'
import { workspaceApi } from '@/api/workspaces'
import { useWorkspaceStore } from '@/stores/workspaceStore'

export default function SetupPage() {
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [error, setError] = useState('')
  const [loading, setLoading] = useState(false)
  const navigate = useNavigate()
  const { setWorkspaces, setActiveWorkspace } = useWorkspaceStore()

  useEffect(() => {
    workspaceApi.list().then((res) => {
      if (res.data && res.data.length > 0) {
        setWorkspaces(res.data)
        setActiveWorkspace(res.data[0])
        navigate(`/w/${res.data[0].id}`)
      }
    }).catch(() => {})
  }, [navigate, setWorkspaces, setActiveWorkspace])

  const handleSubmit = async (e: React.FormEvent) => {
    e.preventDefault()
    setError('')
    setLoading(true)

    try {
      const res = await workspaceApi.create({ name, slug })
      setWorkspaces([res.data])
      setActiveWorkspace(res.data)
      navigate(`/w/${res.data.id}`)
    } catch (err) {
      setError(err instanceof Error ? err.message : 'Failed to create workspace')
    } finally {
      setLoading(false)
    }
  }

  return (
    <div className="min-h-screen flex items-center justify-center bg-surface-950 px-4">
      <div className="w-full max-w-sm">
        <div className="text-center mb-8">
          <h1 className="text-2xl font-bold text-white">Create a Workspace</h1>
          <p className="text-surface-200 mt-1">Set up your team's workspace</p>
        </div>

        <form onSubmit={handleSubmit} className="space-y-4">
          {error && (
            <div className="bg-red-500/10 border border-red-500/20 rounded-lg p-3 text-red-400 text-sm">{error}</div>
          )}
          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">Workspace Name</label>
            <input type="text" value={name} onChange={(e) => { setName(e.target.value); setSlug(e.target.value.toLowerCase().replace(/[^a-z0-9]+/g, '-').replace(/^-|-$/g, '')) }}
              required className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="My Team" />
          </div>
          <div>
            <label className="block text-sm font-medium text-surface-200 mb-1">URL Slug</label>
            <input type="text" value={slug} onChange={(e) => setSlug(e.target.value)}
              required className="w-full px-3 py-2 bg-surface-900 border border-surface-700 rounded-lg text-white focus:outline-none focus:ring-2 focus:ring-brand-500" placeholder="my-team" />
          </div>
          <button type="submit" disabled={loading}
            className="w-full py-2.5 bg-brand-600 hover:bg-brand-700 disabled:opacity-50 rounded-lg text-white font-medium transition-colors">
            {loading ? 'Creating...' : 'Create Workspace'}
          </button>
        </form>
      </div>
    </div>
  )
}
