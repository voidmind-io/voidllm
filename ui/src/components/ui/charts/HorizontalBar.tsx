export interface HorizontalBarItem {
  label: string
  value: number
  detail?: string
}

export interface HorizontalBarProps {
  items: HorizontalBarItem[]
  maxValue?: number
  color?: string
}

export function HorizontalBar({ items, maxValue, color }: HorizontalBarProps) {
  const max = maxValue ?? Math.max(...items.map((i) => i.value), 1)

  return (
    <div className="space-y-5">
      {items.map((item, idx) => {
        const pct = max > 0 ? (item.value / max) * 100 : 0
        const opacity = Math.max(1 - idx * 0.2, 0.2)

        const barStyle: React.CSSProperties = color
          ? { width: `${pct}%`, background: color, opacity }
          : {
              width: `${pct}%`,
              background: 'linear-gradient(90deg, #6366f1, #8b5cf6)',
              opacity,
            }

        return (
          <div key={item.label}>
            <div className="flex items-center justify-between mb-1.5">
              <span className="text-sm text-text-secondary truncate mr-2">{item.label}</span>
              {item.detail != null && (
                <span className="text-xs text-text-tertiary shrink-0 tabular-nums">{item.detail}</span>
              )}
            </div>
            <div className="h-2.5 rounded-full bg-[#25252d] overflow-hidden">
              <div className="h-full rounded-full transition-all duration-500" style={barStyle} />
            </div>
          </div>
        )
      })}
    </div>
  )
}
