import { usePresenceStore } from '@/stores/presenceStore'

interface Props {
  channelId: string
}

export default function TypingIndicator({ channelId }: Props) {
  const typingUsers = usePresenceStore((s) => s.typingUsers[channelId] || [])

  if (typingUsers.length === 0) return null

  const text =
    typingUsers.length === 1
      ? `${typingUsers[0].slice(0, 8)} is typing...`
      : typingUsers.length === 2
        ? `${typingUsers[0].slice(0, 8)} and ${typingUsers[1].slice(0, 8)} are typing...`
        : `${typingUsers.length} people are typing...`

  return (
    <div className="px-5 py-1 text-xs text-surface-200/60 animate-pulse">
      {text}
    </div>
  )
}
