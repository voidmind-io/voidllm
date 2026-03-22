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
    <div className="flex gap-0 border-b border-border mb-6">
      {tabs.map(tab => (
        <NavLink
          key={tab.path}
          to={tab.path}
          end={tab.end ?? true}
          className={({ isActive }) =>
            cn(
              'px-4 py-2.5 text-sm font-medium no-underline border-b-2 -mb-px transition-colors duration-200',
              isActive
                ? 'border-accent text-accent'
                : 'border-transparent text-text-tertiary hover:text-text-secondary'
            )
          }
        >
          {tab.label}
        </NavLink>
      ))}
    </div>
  )
}
