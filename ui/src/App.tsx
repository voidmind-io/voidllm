import { BrowserRouter, Routes, Route, Navigate } from 'react-router-dom'
import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
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
import SystemUsersPage from './pages/SystemUsersPage'
import MCPServersPage from './pages/MCPServersPage'
import { ToastProvider } from './hooks/useToast'
import { Shell } from './components/layout/Shell'
import { PageHeader } from './components/ui/PageHeader'
import { LOCAL_STORAGE_KEY } from './lib/constants'

const queryClient = new QueryClient({
  defaultOptions: {
    queries: {
      retry: 1,
      staleTime: 30_000,
    },
  },
})

function PlaceholderPage({ title, description }: { title: string; description?: string }) {
  return (
    <>
      <PageHeader title={title} description={description} />
      <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
        <p className="text-sm text-text-tertiary">Coming soon</p>
      </div>
    </>
  )
}

function RequireAuth() {
  const token = localStorage.getItem(LOCAL_STORAGE_KEY)
  if (!token) return <Navigate to="/login" replace />
  return <Shell />
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
                <Route index element={<Navigate to="users" replace />} />
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
              <Route
                path="*"
                element={
                  <PlaceholderPage
                    title="Not Found"
                    description="This page does not exist."
                  />
                }
              />
            </Route>
          </Routes>
        </BrowserRouter>
      </ToastProvider>
    </QueryClientProvider>
  )
}
