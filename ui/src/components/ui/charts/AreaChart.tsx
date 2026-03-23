import {
  ResponsiveContainer,
  AreaChart as RechartsAreaChart,
  Area,
  XAxis,
  YAxis,
  Tooltip,
  CartesianGrid,
} from 'recharts'
import type { TooltipContentProps } from 'recharts'
import type { ValueType, NameType } from 'recharts/types/component/DefaultTooltipContent'

export interface AreaChartProps {
  data: { label: string; value: number }[]
  height?: number
  color?: string
  showGrid?: boolean
  formatValue?: (n: number) => string
}

function renderTooltip(
  props: TooltipContentProps<ValueType, NameType>,
  formatValue?: (n: number) => string,
) {
  const { active, payload, label } = props
  if (!active || !payload || payload.length === 0) return null
  const raw = payload[0]?.value
  const numVal = typeof raw === 'number' ? raw : 0
  const display = formatValue ? formatValue(numVal) : numVal.toLocaleString()
  return (
    <div
      style={{
        background: '#1a1a24',
        border: '1px solid rgba(255,255,255,0.1)',
        borderRadius: 8,
        padding: '8px 12px',
      }}
    >
      <p style={{ color: '#8494a8', fontSize: 11, marginBottom: 2 }}>{label}</p>
      <p style={{ color: '#e2e8f0', fontSize: 14, fontWeight: 600 }}>{display}</p>
    </div>
  )
}

const GRADIENT_ID_PREFIX = 'area-chart-gradient-'

export function AreaChart({
  data,
  height = 300,
  color = '#8b5cf6',
  showGrid = false,
  formatValue,
}: AreaChartProps) {
  // Derive a stable ID from color so multiple charts on the same page can coexist
  const gradientId = `${GRADIENT_ID_PREFIX}${color.replace(/[^a-z0-9]/gi, '')}`

  const chartData = data.map((d) => ({ label: d.label, value: d.value }))

  return (
    <ResponsiveContainer width="100%" height={height}>
      <RechartsAreaChart data={chartData} margin={{ top: 4, right: 4, left: 0, bottom: 0 }}>
        <defs>
          <linearGradient id={gradientId} x1="0" y1="0" x2="0" y2="1">
            <stop offset="0%" stopColor={color} stopOpacity={0.3} />
            <stop offset="100%" stopColor={color} stopOpacity={0} />
          </linearGradient>
        </defs>

        {showGrid && (
          <CartesianGrid
            strokeDasharray="3 3"
            stroke="rgba(255,255,255,0.05)"
            vertical={false}
          />
        )}

        <XAxis
          dataKey="label"
          tick={{ fill: '#8494a8', fontSize: 11 }}
          axisLine={false}
          tickLine={false}
        />

        <YAxis hide />

        <Tooltip
          content={(tooltipProps) => renderTooltip(tooltipProps, formatValue)}
          cursor={{ stroke: 'rgba(255,255,255,0.08)', strokeWidth: 1 }}
        />

        <Area
          type="monotone"
          dataKey="value"
          stroke={color}
          strokeWidth={2}
          fill={`url(#${gradientId})`}
          dot={false}
          activeDot={{ r: 4, fill: color, stroke: '#1a1a24', strokeWidth: 2 }}
        />
      </RechartsAreaChart>
    </ResponsiveContainer>
  )
}
