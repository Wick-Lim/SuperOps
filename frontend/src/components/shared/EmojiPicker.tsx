const COMMON_EMOJIS = [
  '👍', '👎', '❤️', '😂', '😮', '😢', '🎉', '🔥',
  '👀', '🚀', '💯', '✅', '❌', '⭐', '🙏', '👏',
]

interface Props {
  onSelect: (emoji: string) => void
}

export default function EmojiPicker({ onSelect }: Props) {
  return (
    <div className="grid grid-cols-8 gap-1 p-2 bg-surface-800 border border-surface-700 rounded-lg shadow-xl">
      {COMMON_EMOJIS.map((emoji) => (
        <button key={emoji} onClick={() => onSelect(emoji)}
          className="w-8 h-8 flex items-center justify-center text-lg hover:bg-surface-700 rounded transition-colors">
          {emoji}
        </button>
      ))}
    </div>
  )
}
