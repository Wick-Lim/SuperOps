interface Props {
  count: number
  max?: number
}

export default function Badge({ count, max = 99 }: Props) {
  if (count <= 0) return null
  const display = count > max ? `${max}+` : String(count)
  return (
    <span className="inline-flex items-center justify-center min-w-[18px] h-[18px] px-1 bg-red-500 text-white text-[10px] font-bold rounded-full">
      {display}
    </span>
  )
}
