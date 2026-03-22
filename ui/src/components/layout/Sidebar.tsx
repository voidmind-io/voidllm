import { useMemo } from 'react'
import { NavLink, Link } from 'react-router-dom'
import { useQueryClient } from '@tanstack/react-query'
import { useMe } from '../../hooks/useMe'
import { useLicense } from '../../hooks/useLicense'
import { LOCAL_STORAGE_KEY } from '../../lib/constants'

function formatRole(role?: string): string {
  if (!role) return '...'
  return role.split('_').map(w => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')
}

interface NavItem {
  label: string
  path: string
  icon: React.ReactNode
  locked?: boolean
  minRole?: string
  end?: boolean
}

interface NavGroup {
  label: string
  items: NavItem[]
  minRole?: string
}

const roleLevel: Record<string, number> = {
  member: 0,
  team_admin: 1,
  org_admin: 2,
  system_admin: 3,
}

function hasMinRole(userRole: string, minRole?: string): boolean {
  if (!minRole) return true
  if (import.meta.env.DEV && !(userRole in roleLevel)) {
    console.warn(`[Sidebar] Unknown role "${userRole}" — defaulting to member visibility`)
  }
  return (roleLevel[userRole] ?? 0) >= (roleLevel[minRole] ?? 0)
}

const iconProps = {
  className: 'h-5 w-5 shrink-0',
  viewBox: '0 0 24 24',
  fill: 'none',
  stroke: 'currentColor',
  strokeWidth: 1.5,
  strokeLinecap: 'round' as const,
  strokeLinejoin: 'round' as const,
  'aria-hidden': true,
}

function IconDashboard() {
  return (
    <svg {...iconProps}>
      <rect x="3" y="3" width="7" height="7" rx="1" />
      <rect x="14" y="3" width="7" height="7" rx="1" />
      <rect x="3" y="14" width="7" height="7" rx="1" />
      <rect x="14" y="14" width="7" height="7" rx="1" />
    </svg>
  )
}

function IconTerminal() {
  return (
    <svg {...iconProps}>
      <polyline points="4 17 10 11 4 5" />
      <line x1="12" y1="19" x2="20" y2="19" />
    </svg>
  )
}

function IconKey() {
  return (
    <svg {...iconProps}>
      <path d="M21 2l-2 2m-7.61 7.61a5.5 5.5 0 1 1-7.778 7.778 5.5 5.5 0 0 1 7.777-7.777zm0 0L15.5 7.5m0 0l3 3L22 7l-3-3m-3.5 3.5L19 4" />
    </svg>
  )
}

function IconUsers() {
  return (
    <svg {...iconProps}>
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  )
}

function IconBot() {
  return (
    <svg {...iconProps}>
      <rect x="3" y="11" width="18" height="10" rx="2" />
      <circle cx="12" cy="5" r="3" />
      <line x1="12" y1="8" x2="12" y2="11" />
      <line x1="8" y1="16" x2="8" y2="16.01" />
      <line x1="16" y1="16" x2="16" y2="16.01" />
    </svg>
  )
}

function IconCube() {
  return (
    <svg {...iconProps}>
      <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z" />
      <polyline points="3.27 6.96 12 12.01 20.73 6.96" />
      <line x1="12" y1="22.08" x2="12" y2="12" />
    </svg>
  )
}

function IconBarChart() {
  return (
    <svg {...iconProps}>
      <line x1="18" y1="20" x2="18" y2="10" />
      <line x1="12" y1="20" x2="12" y2="4" />
      <line x1="6" y1="20" x2="6" y2="14" />
    </svg>
  )
}

function IconDollar() {
  return (
    <svg {...iconProps}>
      <line x1="12" y1="1" x2="12" y2="23" />
      <path d="M17 5H9.5a3.5 3.5 0 0 0 0 7h5a3.5 3.5 0 0 1 0 7H6" />
    </svg>
  )
}

function IconClipboard() {
  return (
    <svg {...iconProps}>
      <path d="M16 4h2a2 2 0 0 1 2 2v14a2 2 0 0 1-2 2H6a2 2 0 0 1-2-2V6a2 2 0 0 1 2-2h2" />
      <rect x="8" y="2" width="8" height="4" rx="1" ry="1" />
      <line x1="8" y1="10" x2="16" y2="10" />
      <line x1="8" y1="14" x2="16" y2="14" />
      <line x1="8" y1="18" x2="12" y2="18" />
    </svg>
  )
}

function IconShield() {
  return (
    <svg {...iconProps}>
      <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
    </svg>
  )
}

function IconBuilding() {
  return (
    <svg {...iconProps}>
      <rect x="4" y="2" width="16" height="20" rx="2" ry="2" />
      <line x1="9" y1="22" x2="9" y2="2" />
      <line x1="15" y1="22" x2="15" y2="2" />
      <line x1="4" y1="12" x2="20" y2="12" />
      <line x1="4" y1="7" x2="20" y2="7" />
      <line x1="4" y1="17" x2="20" y2="17" />
    </svg>
  )
}

function IconPersonPlus() {
  return (
    <svg {...iconProps}>
      <path d="M16 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="8.5" cy="7" r="4" />
      <line x1="20" y1="8" x2="20" y2="14" />
      <line x1="23" y1="11" x2="17" y2="11" />
    </svg>
  )
}

function buildNavigation(hasFeature: (f: string) => boolean): NavGroup[] {
  return [
    {
      label: 'Overview',
      items: [
        { label: 'Dashboard', path: '/', icon: <IconDashboard /> },
        { label: 'Playground', path: '/playground', icon: <IconTerminal /> },
      ],
    },
    {
      label: 'Manage',
      items: [
        { label: 'Keys', path: '/keys', icon: <IconKey /> },
        { label: 'Teams', path: '/teams', icon: <IconUsers />, minRole: 'team_admin', end: false },
        { label: 'Service Accounts', path: '/service-accounts', icon: <IconBot /> },
      ],
    },
    {
      label: 'Analytics',
      items: [
        { label: 'Usage', path: '/usage', icon: <IconBarChart /> },
        { label: 'Cost Reports', path: '/cost-reports', icon: <IconDollar />, locked: !hasFeature('cost_reports') },
      ],
    },
    {
      label: 'Security',
      minRole: 'org_admin',
      items: [
        { label: 'Audit Log', path: '/audit-log', icon: <IconClipboard />, locked: !hasFeature('audit_logs') },
        { label: 'SSO Config', path: '/sso', icon: <IconShield />, locked: !hasFeature('sso_oidc') },
      ],
    },
    {
      label: '',
      items: [
        { label: 'Organization', path: '/org', icon: <IconBuilding />, end: false },
      ],
    },
    {
      label: 'System',
      minRole: 'system_admin',
      items: [
        { label: 'Organizations', path: '/orgs', icon: <IconBuilding />, end: false },
        { label: 'Users', path: '/users', icon: <IconPersonPlus /> },
        { label: 'Models', path: '/models', icon: <IconCube />, minRole: 'system_admin' },
        { label: 'License', path: '/license', icon: <IconShield /> },
      ],
    },
  ]
}

function LockIcon() {
  return (
    <svg
      aria-hidden="true"
      className="h-3 w-3 shrink-0 opacity-50"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2.5}
    >
      <path
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"
      />
    </svg>
  )
}

export function Sidebar() {
  const { data } = useMe()
  const { data: license } = useLicense()
  const queryClient = useQueryClient()

  const userRole = data?.role ?? 'member'
  const hasFeature = (f: string): boolean => license?.features?.includes(f) ?? false

  const visibleGroups = useMemo(() => {
    const navigation = buildNavigation(hasFeature)
    return navigation
      .filter(group => hasMinRole(userRole, group.minRole))
      .map(group => ({
        ...group,
        items: group.items.filter(item => hasMinRole(userRole, item.minRole)),
      }))
      .filter(group => group.items.length > 0)
  // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [userRole, license])

  return (
    <aside
      aria-label="Main navigation"
      className="w-[260px] bg-bg-secondary border-r border-white/5 flex flex-col fixed h-screen z-50"
    >
      {/* Logo */}
      <div className="px-4 py-4 border-b border-white/5 shrink-0">
        <a href="/" className="flex items-center gap-2 no-underline">
          <img src="/logo.svg" alt="VoidLLM" className="h-7 w-7" />
          <span className="gradient-text text-xl font-bold">VoidLLM</span>
        </a>
      </div>

      {/* Navigation */}
      <nav className="flex-1 flex flex-col gap-0.5 p-3 overflow-y-auto">
        {visibleGroups.map((group, groupIndex) => (
            <div key={group.label || `group-${groupIndex}`}>
              {groupIndex > 0 && (
                <div className="h-px bg-white/10 my-2" />
              )}
              {group.label && (
                <div className="text-[11px] uppercase tracking-wider text-text-tertiary/50 px-3 mb-1 mt-1">
                  {group.label}
                </div>
              )}
              {group.items.map((item) => (
                <NavLink
                  key={item.path}
                  to={item.path}
                  end={item.end !== undefined ? item.end : item.path === '/'}
                  className={({ isActive }) =>
                    [
                      'flex items-center gap-3 px-3 py-2 rounded-lg text-sm no-underline transition-all duration-200',
                      isActive
                        ? 'bg-accent/15 text-accent'
                        : item.locked
                          ? 'text-text-tertiary/40 hover:bg-bg-tertiary hover:text-text-tertiary'
                          : 'text-text-secondary hover:bg-bg-tertiary hover:text-text-primary',
                    ].join(' ')
                  }
                >
                  {item.icon}
                  <span className="flex-1">{item.label}</span>
                  {item.locked && <LockIcon />}
                </NavLink>
              ))}
            </div>
          ))}
      </nav>

      {/* Footer */}
      <div className="shrink-0 border-t border-white/5 p-3">
        <div className="flex items-center justify-between mb-2">
          <Link
            to="/profile"
            className="text-xs text-text-secondary truncate max-w-[140px] hover:text-text-primary transition-colors no-underline"
            title="View profile"
          >
            {data?.display_name || data?.email || '...'}
          </Link>
          <span className="rounded bg-accent/15 px-1.5 py-0.5 text-[10px] font-semibold text-accent uppercase">{formatRole(data?.role)}</span>
        </div>
        <div className="flex gap-2">
          <Link
            to="/profile"
            className="flex-1 py-1.5 bg-transparent border border-white/10 rounded-md text-xs text-text-secondary cursor-pointer transition-colors duration-200 hover:border-accent/40 hover:text-text-primary text-center no-underline"
          >
            Profile
          </Link>
          <button
            onClick={() => {
              localStorage.removeItem(LOCAL_STORAGE_KEY)
              queryClient.clear()
              window.location.href = '/login'
            }}
            className="flex-1 py-1.5 bg-transparent border border-white/10 rounded-md text-xs text-text-secondary cursor-pointer transition-colors duration-200 hover:border-error hover:text-error"
          >
            Logout
          </button>
        </div>
      </div>
    </aside>
  )
}
