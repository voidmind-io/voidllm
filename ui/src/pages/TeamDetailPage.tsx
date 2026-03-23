import { useParams, Outlet, Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Tabs } from '../components/ui/Tabs'
import { useMe } from '../hooks/useMe'
import { useTeam } from '../hooks/useTeams'
import type { Tab } from '../components/ui/Tabs'

export default function TeamDetailPage() {
  const { teamId = '' } = useParams<{ teamId: string }>()
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const { data: team } = useTeam(orgId, teamId)

  if (!teamId) {
    return <Navigate to="/teams" replace />
  }

  const tabs: Tab[] = [
    { label: 'Members', path: `/teams/${teamId}/members` },
    { label: 'Models', path: `/teams/${teamId}/models` },
    { label: 'Settings', path: `/teams/${teamId}/settings` },
  ]

  return (
    <>
      <PageHeader
        title={team?.name ?? 'Team'}
        description={team?.slug ? `Slug: ${team.slug}` : 'Manage team settings and access'}
      />
      <Tabs tabs={tabs} />
      <Outlet />
    </>
  )
}
