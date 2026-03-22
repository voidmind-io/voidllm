import { useParams, Outlet, Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Tabs } from '../components/ui/Tabs'
import { useOrg } from '../hooks/useOrg'
import { useMe } from '../hooks/useMe'
import type { Tab } from '../components/ui/Tabs'

export default function OrgDetailPage() {
  const { orgId = '' } = useParams<{ orgId: string }>()

  const { data: me } = useMe()
  const { data: org } = useOrg(orgId)

  if (!orgId) {
    return <Navigate to="/orgs" replace />
  }

  if (me && !me.is_system_admin) {
    return <Navigate to="/" replace />
  }

  const tabs: Tab[] = [
    { label: 'Members', path: `/orgs/${orgId}/members` },
    { label: 'Teams', path: `/orgs/${orgId}/teams` },
    { label: 'Settings', path: `/orgs/${orgId}/settings` },
    { label: 'SSO', path: `/orgs/${orgId}/sso` },
  ]

  return (
    <>
      <PageHeader
        title={org?.name ?? 'Organization'}
        description="Manage organization settings, members, and teams"
      />
      <Tabs tabs={tabs} />
      <Outlet />
    </>
  )
}
