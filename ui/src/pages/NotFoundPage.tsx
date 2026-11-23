import { Link, useLocation } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'

/** Fallback for unknown routes inside the authenticated shell. */
export default function NotFoundPage() {
  const { pathname } = useLocation()
  return (
    <>
      <PageHeader title="Page not found" />
      <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
        <p className="text-sm text-text-secondary">
          Nothing exists at{' '}
          <code className="font-mono text-text-primary break-all">{pathname}</code>
        </p>
        <Link
          to="/"
          className="mt-4 inline-block text-sm text-accent hover:underline focus-visible:outline-none focus-visible:ring-2 focus-visible:ring-accent rounded-sm"
        >
          Go to dashboard
        </Link>
      </div>
    </>
  )
}
