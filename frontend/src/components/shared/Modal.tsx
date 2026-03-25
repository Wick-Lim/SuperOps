import { useEffect, useRef, type ReactNode } from 'react'

interface Props {
  open: boolean
  onClose: () => void
  title?: string
  children: ReactNode
  width?: string
}

export default function Modal({ open, onClose, title, children, width = 'max-w-lg' }: Props) {
  const overlayRef = useRef<HTMLDivElement>(null)

  useEffect(() => {
    const handleEsc = (e: KeyboardEvent) => { if (e.key === 'Escape') onClose() }
    if (open) document.addEventListener('keydown', handleEsc)
    return () => document.removeEventListener('keydown', handleEsc)
  }, [open, onClose])

  if (!open) return null

  return (
    <div ref={overlayRef} className="fixed inset-0 z-50 flex items-center justify-center bg-black/60 backdrop-blur-sm"
      onClick={(e) => { if (e.target === overlayRef.current) onClose() }}>
      <div className={`${width} w-full mx-4 bg-surface-900 border border-surface-700/50 rounded-xl shadow-2xl`}>
        {title && (
          <div className="flex items-center justify-between px-5 py-4 border-b border-surface-700/50">
            <h2 className="font-semibold text-white">{title}</h2>
            <button onClick={onClose} className="text-surface-200 hover:text-white text-lg">&times;</button>
          </div>
        )}
        <div className="p-5">{children}</div>
      </div>
    </div>
  )
}
