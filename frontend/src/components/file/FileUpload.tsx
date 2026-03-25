import { useRef, useState } from 'react'
import { fileApi } from '@/api/files'
import { useWorkspaceStore } from '@/stores/workspaceStore'

interface Props {
  onUploaded: (file: { id: string; name: string; content_type: string }) => void
}

export default function FileUpload({ onUploaded }: Props) {
  const inputRef = useRef<HTMLInputElement>(null)
  const [uploading, setUploading] = useState(false)
  const [progress, setProgress] = useState('')
  const workspace = useWorkspaceStore((s) => s.activeWorkspace)

  const handleChange = async (e: React.ChangeEvent<HTMLInputElement>) => {
    const file = e.target.files?.[0]
    if (!file || !workspace) return

    setUploading(true)
    setProgress(file.name)
    try {
      const result = await fileApi.upload(workspace.id, file)
      onUploaded(result)
    } catch {
      // ignore
    } finally {
      setUploading(false)
      setProgress('')
      if (inputRef.current) inputRef.current.value = ''
    }
  }

  return (
    <>
      <input ref={inputRef} type="file" onChange={handleChange} className="hidden" />
      <button
        onClick={() => inputRef.current?.click()}
        disabled={uploading}
        className="w-8 h-8 flex items-center justify-center text-surface-200/60 hover:text-white hover:bg-surface-800 rounded-lg transition-colors text-lg"
        title="Attach file"
      >
        {uploading ? (
          <span className="w-4 h-4 border-2 border-current border-t-transparent rounded-full animate-spin" />
        ) : '+'}
      </button>
      {progress && <span className="text-xs text-surface-200/50 ml-1 truncate max-w-32">{progress}</span>}
    </>
  )
}
