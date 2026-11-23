import { NavLink, Outlet } from 'react-router-dom'
import { PageHeader } from '../../components/ui/PageHeader'
import { useMe } from '../../hooks/useMe'
import { isOrgAdmin } from '../../lib/roles'

const tabs: { path: string; label: string; exact?: boolean; minRole?: string }[] = [
  { path: '/usage', label: 'Overview', exact: true },
  { path: '/usage/llm', label: 'LLM' },
  { path: '/usage/mcp', label: 'MCP', minRole: 'org_admin' },
]

export default function UsageLayout() {
  const { data: me } = useMe()
  const visibleTabs = tabs.filter((tab) => !tab.minRole || isOrgAdmin(me))

  return (
    <>
      <PageHeader
        title="Usage"
        description={
          me && !isOrgAdmin(me)
            ? 'Your usage'
            : 'Track token usage and costs across LLM and MCP'
        }
      />

      <div className="flex items-center gap-1 mb-6 border-b border-border">
        {visibleTabs.map((tab) => (
          <NavLink
            key={tab.path}
            to={tab.path}
            end={tab.exact}
            className={({ isActive }) =>
              `px-4 py-2.5 text-sm font-medium transition-colors -mb-px ${
                isActive
                  ? 'text-accent border-b-2 border-accent'
                  : 'text-text-secondary hover:text-text-primary'
              }`
            }
          >
            {tab.label}
          </NavLink>
        ))}
      </div>

      <Outlet />
    </>
  )
}
