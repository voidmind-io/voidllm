import { Outlet } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Tabs } from '../components/ui/Tabs'
import { useMe } from '../hooks/useMe'
import type { Tab } from '../components/ui/Tabs'

export default function OrganizationPage() {
  const { data: me } = useMe()

  const isOrgAdmin = me?.role === 'org_admin' || me?.role === 'system_admin'

  const tabs: Tab[] = [
    ...(isOrgAdmin
      ? [
          { label: 'Members', path: '/org/users' },
          { label: 'Models', path: '/org/models' },
        ]
      : []),
    { label: 'Settings', path: '/org/settings' },
  ]

  return (
    <>
      <PageHeader title="Organization" description="Manage your organization" />
      <Tabs tabs={tabs} />
      <Outlet />
    </>
  )
}
