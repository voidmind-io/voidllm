import { cn } from '../../lib/utils'

export interface Tab {
  key: string
  label: string
}

export interface TabSwitcherProps {
  tabs: Tab[]
  activeKey: string
  onChange: (key: string) => void
}

export default function TabSwitcher({ tabs, activeKey, onChange }: TabSwitcherProps) {
  return (
    <div role="tablist" className="inline-flex gap-1 rounded-lg bg-bg-tertiary p-1 mb-6">
      {tabs.map((tab) => (
        <button
          key={tab.key}
          type="button"
          role="tab"
          aria-selected={tab.key === activeKey}
          onClick={() => onChange(tab.key)}
          className={cn(
            'px-4 py-2 text-sm font-medium rounded-md transition-all duration-200',
            tab.key === activeKey
              ? 'bg-bg-secondary text-text-primary shadow-sm'
              : 'text-text-tertiary hover:text-text-secondary'
          )}
        >
          {tab.label}
        </button>
      ))}
    </div>
  )
}
