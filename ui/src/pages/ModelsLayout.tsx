import { PageHeader } from '../components/ui/PageHeader'
import { useMe } from '../hooks/useMe'
import ModelsPage from './ModelsPage'

export default function ModelsLayout() {
  const { data: me } = useMe()

  if (me && !me.is_system_admin) {
    return (
      <>
        <PageHeader title="Models" description="System model registry" />
        <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
          <p className="text-sm text-text-tertiary">You need system admin permissions to manage models.</p>
        </div>
      </>
    )
  }

  return <ModelsPage />
}
