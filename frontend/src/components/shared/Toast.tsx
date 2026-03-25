import { create } from 'zustand'
import { useEffect } from 'react'

interface ToastItem {
  id: number
  message: string
  type: 'success' | 'error' | 'info'
}

interface ToastState {
  toasts: ToastItem[]
  add: (message: string, type?: ToastItem['type']) => void
  remove: (id: number) => void
}

let nextId = 0

export const useToastStore = create<ToastState>()((set) => ({
  toasts: [],
  add: (message, type = 'info') => {
    const id = ++nextId
    set((s) => ({ toasts: [...s.toasts, { id, message, type }] }))
    setTimeout(() => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })), 4000)
  },
  remove: (id) => set((s) => ({ toasts: s.toasts.filter((t) => t.id !== id) })),
}))

const colors = {
  success: 'bg-green-600 border-green-500',
  error: 'bg-red-600 border-red-500',
  info: 'bg-brand-600 border-brand-500',
}

export default function ToastContainer() {
  const toasts = useToastStore((s) => s.toasts)
  const remove = useToastStore((s) => s.remove)

  if (toasts.length === 0) return null

  return (
    <div className="fixed bottom-4 right-4 z-[100] flex flex-col gap-2">
      {toasts.map((t) => (
        <div key={t.id} onClick={() => remove(t.id)}
          className={`px-4 py-3 rounded-lg text-white text-sm border shadow-lg cursor-pointer animate-[slideIn_0.2s_ease] ${colors[t.type]}`}>
          {t.message}
        </div>
      ))}
    </div>
  )
}
