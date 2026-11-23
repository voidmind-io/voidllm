import { useEffect, useState } from 'react'
import { BrowserRouter, Routes, Route, Navigate, useLocation } from 'react-router-dom'
import { QueryClientProvider } from '@tanstack/react-query'
import { queryClient } from './lib/queryClient'
import { useMe } from './hooks/useMe'
import { isOrgAdmin } from './lib/roles'
import LoginPage from './pages/auth/LoginPage'
import CallbackPage from './pages/auth/CallbackPage'
import AcceptInvitePage from './pages/AcceptInvitePage'
import DashboardPage from './pages/DashboardPage'
import KeysPage from './pages/KeysPage'
import TeamsPage from './pages/TeamsPage'
import TeamDetailPage from './pages/TeamDetailPage'
import TeamMembersTab from './pages/TeamMembersTab'
import TeamModelsTab from './pages/TeamModelsTab'
import TeamSettingsTab from './pages/TeamSettingsTab'
import TeamMCPAccessTab from './pages/TeamMCPAccessTab'
import OrganizationPage from './pages/OrganizationPage'
import OrgUsersPage from './pages/OrgUsersPage'
import OrganizationsPage from './pages/OrganizationsPage'
import OrgDetailPage from './pages/OrgDetailPage'
import OrgDetailMembersTab from './pages/OrgDetailMembersTab'
import OrgDetailTeamsTab from './pages/OrgDetailTeamsTab'
import OrgDetailSettingsTab from './pages/OrgDetailSettingsTab'
import OrgDetailSSOTab from './pages/OrgDetailSSOTab'
import SSOConfigPage from './pages/SSOConfigPage'
import ServiceAccountsPage from './pages/ServiceAccountsPage'
import ModelsLayout from './pages/ModelsLayout'
import ModelsAccessTab from './pages/ModelsAccessTab'
import MCPAccessTab from './pages/MCPAccessTab'
import SettingsPage from './pages/SettingsPage'
import LicensePage from './pages/LicensePage'
import UsageLayout from './pages/usage/UsageLayout'
import UsageOverviewPage from './pages/usage/UsageOverviewPage'
import LLMUsagePage from './pages/usage/LLMUsagePage'
import MCPUsagePage from './pages/usage/MCPUsagePage'
import CostReportsPage from './pages/CostReportsPage'
import ProfilePage from './pages/ProfilePage'
import AuditLogPage from './pages/AuditLogPage'
import PlaygroundPage from './pages/PlaygroundPage'
import NotFoundPage from './pages/NotFoundPage'
import SystemUsersPage from './pages/SystemUsersPage'
import MCPServersPage from './pages/MCPServersPage'
import { ToastProvider, useToast } from './hooks/useToast'
import { Shell } from './components/layout/Shell'
import { LOCAL_STORAGE_KEY } from './lib/constants'
import {
  clearSession,
  getSessionExpiryMs,
  isSessionExpired,
  msUntilSessionExpiry,
} from './lib/session'

/** How long before expiry the user is warned. */
const EXPIRY_WARNING_MS = 5 * 60 * 1000
/** Largest delay setTimeout accepts. */
const MAX_TIMEOUT_MS = 2_147_483_647

export function RequireAuth() {
  const token = localStorage.getItem(LOCAL_STORAGE_KEY)
  const [expired, setExpired] = useState(() => isSessionExpired())
  const location = useLocation()
  const { toast } = useToast()

  useEffect(() => {
    if (!token) return
    let timer: number | undefined
    const check = () => {
      if (isSessionExpired()) {
        clearSession()
        queryClient.clear()
        setExpired(true)
        return
      }
      const delay = msUntilSessionExpiry()
      if (delay !== null) timer = window.setTimeout(check, delay)
    }
    check()
    return () => window.clearTimeout(timer)
  }, [token])

  useEffect(() => {
    if (!token) return
    let timer: number | undefined
    const schedule = () => {
      const expiry = getSessionExpiryMs()
      if (expiry === null) return
      const remaining = expiry - Date.now()
      if (remaining <= 0) return
      if (remaining <= EXPIRY_WARNING_MS) {
        toast({
          variant: 'info',
          duration: 0,
          message: 'Your session ends in 5 minutes. Save your work and sign in again.',
        })
        return
      }
      timer = window.setTimeout(schedule, Math.min(remaining - EXPIRY_WARNING_MS, MAX_TIMEOUT_MS))
    }
    schedule()
    return () => window.clearTimeout(timer)
  }, [token, toast])

  if (!token || expired) {
    const next = location.pathname + location.search
    const target =
      location.pathname.startsWith('/login') || next === '/'
        ? '/login'
        : `/login?next=${encodeURIComponent(next)}`
    return <Navigate to={target} replace />
  }
  return <Shell />
}

/** Sends org admins to the user list and everyone else to the settings tab. */
function OrgIndexRedirect() {
  const { data: me } = useMe()
  if (!me) return null
  return <Navigate to={isOrgAdmin(me) ? 'users' : 'settings'} replace />
}

export default function App() {
  return (
    <QueryClientProvider client={queryClient}>
      <ToastProvider>
        <BrowserRouter>
          <Routes>
            <Route path="/login" element={<LoginPage />} />
            <Route path="/auth/callback" element={<CallbackPage />} />
            <Route path="/invite/:token" element={<AcceptInvitePage />} />
            <Route element={<RequireAuth />}>
              <Route index element={<DashboardPage />} />
              <Route path="playground" element={<PlaygroundPage />} />
              <Route path="keys" element={<KeysPage />} />
              <Route path="teams" element={<TeamsPage />} />
              <Route path="teams/:teamId" element={<TeamDetailPage />}>
                <Route index element={<Navigate to="members" replace />} />
                <Route path="members" element={<TeamMembersTab />} />
                <Route path="models" element={<TeamModelsTab />} />
                <Route path="mcp-access" element={<TeamMCPAccessTab />} />
                <Route path="settings" element={<TeamSettingsTab />} />
              </Route>
              <Route path="org" element={<OrganizationPage />}>
                <Route index element={<OrgIndexRedirect />} />
                <Route path="settings" element={<SettingsPage />} />
                <Route path="users" element={<OrgUsersPage />} />
                <Route path="models" element={<ModelsAccessTab />} />
                <Route path="mcp-access" element={<MCPAccessTab />} />
              </Route>
              <Route path="service-accounts" element={<ServiceAccountsPage />} />
              <Route path="models" element={<ModelsLayout />} />
              <Route path="usage" element={<UsageLayout />}>
                <Route index element={<UsageOverviewPage />} />
                <Route path="llm" element={<LLMUsagePage />} />
                <Route path="mcp" element={<MCPUsagePage />} />
              </Route>
              <Route path="cost-reports" element={<CostReportsPage />} />
              <Route path="profile" element={<ProfilePage />} />
              <Route path="license" element={<LicensePage />} />
              <Route path="audit-log" element={<AuditLogPage />} />
              <Route path="sso" element={<SSOConfigPage />} />
              <Route path="orgs" element={<OrganizationsPage />} />
              <Route path="orgs/:orgId" element={<OrgDetailPage />}>
                <Route index element={<Navigate to="members" replace />} />
                <Route path="members" element={<OrgDetailMembersTab />} />
                <Route path="teams" element={<OrgDetailTeamsTab />} />
                <Route path="settings" element={<OrgDetailSettingsTab />} />
                <Route path="sso" element={<OrgDetailSSOTab />} />
              </Route>
              <Route path="users" element={<SystemUsersPage />} />
              <Route path="mcp-servers" element={<MCPServersPage />} />
              <Route path="*" element={<NotFoundPage />} />
            </Route>
          </Routes>
        </BrowserRouter>
      </ToastProvider>
    </QueryClientProvider>
  )
}
