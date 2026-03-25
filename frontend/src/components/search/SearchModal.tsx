import { useState, useEffect, useRef, useCallback } from 'react'
import { useNavigate } from 'react-router-dom'
import Modal from '@/components/shared/Modal'
import { searchApi, type SearchResult } from '@/api/search'
import { useWorkspaceStore } from '@/stores/workspaceStore'

interface Props {
  open: boolean
  onClose: () => void
}

export default function SearchModal({ open, onClose }: Props) {
  const [query, setQuery] = useState('')
  const [results, setResults] = useState<SearchResult | null>(null)
  const [loading, setLoading] = useState(false)
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)
  const navigate = useNavigate()
  const timer = useRef<ReturnType<typeof setTimeout> | null>(null)

  const doSearch = useCallback(async (q: string) => {
    if (!workspace || !q.trim()) { setResults(null); return }
    setLoading(true)
    try {
      const res = await searchApi.search(workspace.id, q)
      setResults(res.data)
    } catch { setResults(null) }
    finally { setLoading(false) }
  }, [workspace])

  useEffect(() => {
    if (timer.current) clearTimeout(timer.current)
    if (!query.trim()) { setResults(null); return }
    timer.current = setTimeout(() => doSearch(query), 300)
    return () => { if (timer.current) clearTimeout(timer.current) }
  }, [query, doSearch])

  useEffect(() => {
    if (!open) { setQuery(''); setResults(null) }
  }, [open])

  // Global Cmd+K shortcut
  useEffect(() => {
    const handler = (e: KeyboardEvent) => {
      if ((e.metaKey || e.ctrlKey) && e.key === 'k') { e.preventDefault(); onClose() }
    }
    document.addEventListener('keydown', handler)
    return () => document.removeEventListener('keydown', handler)
  }, [onClose])

  const handleSelect = (hit: SearchResult['hits'][0]) => {
    onClose()
    navigate(`/w/${hit.workspace_id}/c/${hit.channel_id}`)
  }

  return (
    <Modal open={open} onClose={onClose} width="max-w-xl">
      <input
        type="text" value={query} onChange={(e) => setQuery(e.target.value)}
        placeholder="Search messages..."
        autoFocus
        className="w-full px-4 py-3 bg-surface-800 border border-surface-700 rounded-lg text-white placeholder-surface-200/40 focus:outline-none focus:ring-1 focus:ring-brand-500 text-sm"
      />

      {loading && <div className="py-8 text-center text-surface-200/50 text-sm">Searching...</div>}

      {results && !loading && (
        <div className="mt-3 max-h-80 overflow-y-auto">
          {results.hits.length === 0 ? (
            <div className="py-8 text-center text-surface-200/50 text-sm">No results found</div>
          ) : (
            <div className="space-y-1">
              {results.hits.map((hit) => (
                <button key={hit.id} onClick={() => handleSelect(hit)}
                  className="w-full text-left px-3 py-2.5 rounded-lg hover:bg-surface-800 transition-colors">
                  <div className="text-sm text-white line-clamp-2">{hit.content}</div>
                  <div className="text-xs text-surface-200/50 mt-1">{hit.user_id.slice(0, 8)} in {hit.channel_id.slice(0, 8)}</div>
                </button>
              ))}
              <div className="text-xs text-surface-200/40 text-center pt-2">
                {results.estimated_total} results ({results.processing_time_ms}ms)
              </div>
            </div>
          )}
        </div>
      )}
    </Modal>
  )
}
