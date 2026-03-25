import { usePresenceStore } from '@/stores/presenceStore'

interface Props {
  userId: string
  size?: 'sm' | 'md'
}

const colors: Record<string, string> = {
  online: 'bg-green-500',
  away: 'bg-yellow-500',
  dnd: 'bg-red-500',
  offline: 'bg-gray-500',
}

export default function PresenceIndicator({ userId, size = 'sm' }: Props) {
  const status = usePresenceStore((s) => s.presence[userId] || 'offline')
  const px = size === 'sm' ? 'w-2.5 h-2.5' : 'w-3 h-3'

  return (
    <span
      className={`inline-block rounded-full border-2 border-surface-900 ${px} ${colors[status] || colors.offline}`}
      title={status}
    />
  )
}
