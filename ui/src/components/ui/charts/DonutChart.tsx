export interface DonutSegment {
  label: string
  value: number
  color: string
}

export interface DonutChartProps {
  segments: DonutSegment[]
  centerLabel?: string
  centerValue?: string
  size?: number
  strokeWidth?: number
}

export function DonutChart({
  segments,
  centerLabel,
  centerValue,
  size = 192,
  strokeWidth = 20,
}: DonutChartProps) {
  const total = segments.reduce((acc, s) => acc + s.value, 0)
  const radius = (size - strokeWidth) / 2
  const circumference = 2 * Math.PI * radius
  const cx = size / 2
  const cy = size / 2

  // Build segments with dasharray / dashoffset using a reduce to avoid mutation inside map
  const renderedSegments = segments
    .reduce<{ seg: DonutSegment; dashArray: number; dashOffset: number; key: number; cumulative: number }[]>(
      (acc, seg, i) => {
        const prevCumulative = acc.length > 0 ? acc[acc.length - 1]!.cumulative : 0
        const percent = total > 0 ? seg.value / total : 0
        const dashArray = circumference * percent
        const dashOffset = circumference * (1 - prevCumulative)
        return [...acc, { seg, dashArray, dashOffset, key: i, cumulative: prevCumulative + percent }]
      },
      [],
    )

  return (
    <div className="flex flex-col items-center gap-4">
      <div className="relative" style={{ width: size, height: size }}>
        <svg width={size} height={size} style={{ transform: 'rotate(-90deg)' }}>
          {/* Background track */}
          <circle
            cx={cx}
            cy={cy}
            r={radius}
            fill="none"
            stroke="#25252d"
            strokeWidth={strokeWidth}
          />
          {renderedSegments.map(({ seg, dashArray, dashOffset, key }) => (
            <circle
              key={key}
              cx={cx}
              cy={cy}
              r={radius}
              fill="none"
              stroke={seg.color}
              strokeWidth={strokeWidth}
              strokeDasharray={`${dashArray} ${circumference - dashArray}`}
              strokeDashoffset={dashOffset}
              strokeLinecap="butt"
            />
          ))}
        </svg>

        {(centerValue != null || centerLabel != null) && (
          <div
            className="absolute inset-0 flex flex-col items-center justify-center text-center"
            aria-hidden="true"
          >
            {centerValue != null && (
              <span className="text-2xl font-semibold text-text-primary leading-tight">
                {centerValue}
              </span>
            )}
            {centerLabel != null && (
              <span className="text-xs text-text-tertiary mt-0.5">{centerLabel}</span>
            )}
          </div>
        )}
      </div>

      {/* Legend */}
      <div className="flex flex-col gap-2 w-full">
        {segments.map((seg, i) => {
          const pct = total > 0 ? Math.round((seg.value / total) * 100) : 0
          return (
            <div key={i} className="flex items-center justify-between text-sm">
              <div className="flex items-center gap-2">
                <span
                  className="block w-2.5 h-2.5 rounded-full shrink-0"
                  style={{ background: seg.color }}
                />
                <span className="text-text-secondary">{seg.label}</span>
              </div>
              <span className="text-text-tertiary tabular-nums">{pct}%</span>
            </div>
          )
        })}
      </div>
    </div>
  )
}
