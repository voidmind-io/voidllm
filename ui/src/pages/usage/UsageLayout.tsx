import { Link, useLocation, Outlet } from 'react-router-dom'
import { PageHeader } from '../../components/ui/PageHeader'

const tabs = [
  { path: '/usage', label: 'Overview', exact: true },
  { path: '/usage/llm', label: 'LLM' },
  { path: '/usage/mcp', label: 'MCP' },
]

export default function UsageLayout() {
  const location = useLocation()

  return (
    <>
      <PageHeader
        title="Usage"
        description="Track token usage and costs across LLM and MCP"
      />

      <div className="flex items-center gap-1 mb-6 border-b border-border">
        {tabs.map((tab) => {
          const isActive = tab.exact
            ? location.pathname === tab.path
            : location.pathname.startsWith(tab.path)
          return (
            <Link
              key={tab.path}
              to={tab.path}
              className={`px-4 py-2.5 text-sm font-medium transition-colors -mb-px ${
                isActive
                  ? 'text-accent border-b-2 border-accent'
                  : 'text-text-secondary hover:text-text-primary'
              }`}
            >
              {tab.label}
            </Link>
          )
        })}
      </div>

      <Outlet />
    </>
  )
}
