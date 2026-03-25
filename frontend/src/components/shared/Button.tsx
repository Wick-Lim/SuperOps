import type { ButtonHTMLAttributes, ReactNode } from 'react'

interface Props extends ButtonHTMLAttributes<HTMLButtonElement> {
  variant?: 'primary' | 'secondary' | 'danger' | 'ghost'
  size?: 'sm' | 'md' | 'lg'
  loading?: boolean
  children: ReactNode
}

const variants = {
  primary: 'bg-brand-600 hover:bg-brand-700 text-white',
  secondary: 'bg-surface-800 hover:bg-surface-700 text-white border border-surface-700',
  danger: 'bg-red-600 hover:bg-red-700 text-white',
  ghost: 'hover:bg-surface-800 text-surface-200 hover:text-white',
}

const sizes = {
  sm: 'px-2.5 py-1 text-xs',
  md: 'px-3.5 py-2 text-sm',
  lg: 'px-5 py-2.5 text-base',
}

export default function Button({ variant = 'primary', size = 'md', loading, children, disabled, className = '', ...props }: Props) {
  return (
    <button
      className={`rounded-lg font-medium transition-colors inline-flex items-center justify-center gap-2 disabled:opacity-50 disabled:cursor-not-allowed ${variants[variant]} ${sizes[size]} ${className}`}
      disabled={disabled || loading}
      {...props}
    >
      {loading && <span className="w-4 h-4 border-2 border-current border-t-transparent rounded-full animate-spin" />}
      {children}
    </button>
  )
}
