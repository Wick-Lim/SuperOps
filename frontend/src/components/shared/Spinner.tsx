interface Props {
  size?: 'sm' | 'md' | 'lg'
}

const sizes = { sm: 'w-4 h-4', md: 'w-6 h-6', lg: 'w-10 h-10' }

export default function Spinner({ size = 'md' }: Props) {
  return <div className={`${sizes[size]} border-2 border-brand-500/30 border-t-brand-500 rounded-full animate-spin`} />
}
