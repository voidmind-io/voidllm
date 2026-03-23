import { NavLink } from 'react-router-dom'
import { cn } from '../../lib/utils'

export interface Tab {
  label: string
  path: string
  end?: boolean
}

export interface TabsProps {
  tabs: Tab[]
}

export function Tabs({ tabs }: TabsProps) {
  return (
    <div className="inline-flex gap-1 rounded-lg bg-bg-tertiary p-1 mb-6">
      {tabs.map(tab => (
        <NavLink
          key={tab.path}
          to={tab.path}
          end={tab.end ?? true}
          className={({ isActive }) =>
            cn(
              'px-4 py-2 text-sm font-medium rounded-md transition-all duration-200 no-underline',
              isActive
                ? 'bg-bg-secondary text-text-primary shadow-sm'
                : 'text-text-tertiary hover:text-text-secondary'
            )
          }
        >
          {tab.label}
        </NavLink>
      ))}
    </div>
  )
}
