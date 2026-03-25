interface Props {
  name: string
  url?: string
  size?: 'sm' | 'md' | 'lg'
  showPresence?: boolean
  online?: boolean
}

const sizeClasses = {
  sm: 'w-7 h-7 text-xs',
  md: 'w-9 h-9 text-sm',
  lg: 'w-12 h-12 text-base',
}

export default function Avatar({ name, url, size = 'md', showPresence, online }: Props) {
  const initials = name.split(' ').map(n => n[0]).join('').slice(0, 2).toUpperCase()
  const color = `hsl(${name.split('').reduce((a, c) => a + c.charCodeAt(0), 0) % 360}, 55%, 50%)`

  return (
    <div className="relative inline-flex shrink-0">
      {url ? (
        <img src={url} alt={name} className={`${sizeClasses[size]} rounded-lg object-cover`} />
      ) : (
        <div className={`${sizeClasses[size]} rounded-lg flex items-center justify-center font-medium text-white`} style={{ backgroundColor: color }}>
          {initials}
        </div>
      )}
      {showPresence && (
        <span className={`absolute -bottom-0.5 -right-0.5 w-3 h-3 rounded-full border-2 border-surface-900 ${online ? 'bg-green-500' : 'bg-gray-500'}`} />
      )}
    </div>
  )
}
