import { PageHeader } from './PageHeader'

export interface UpgradePromptProps {
  title: string
  description: string
}

function LockIcon() {
  return (
    <svg
      aria-hidden="true"
      className="mx-auto h-12 w-12 text-text-tertiary mb-4"
      fill="none"
      viewBox="0 0 24 24"
      stroke="currentColor"
      strokeWidth={2}
    >
      <path
        strokeLinecap="round"
        strokeLinejoin="round"
        d="M12 15v2m-6 4h12a2 2 0 002-2v-6a2 2 0 00-2-2H6a2 2 0 00-2 2v6a2 2 0 002 2zm10-10V7a4 4 0 00-8 0v4h8z"
      />
    </svg>
  )
}

export function UpgradePrompt({ title, description }: UpgradePromptProps) {
  return (
    <>
      <PageHeader title={title} />
      <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
        <div className="mx-auto max-w-md">
          <LockIcon />
          <h2 className="text-lg font-semibold text-text-primary mb-2">
            This feature requires a Pro or Enterprise subscription
          </h2>
          <p className="text-sm text-text-secondary mb-6">{description}</p>
          <div className="flex items-center justify-center gap-3">
            <a
              href="https://voidllm.ai"
              target="_blank"
              rel="noopener noreferrer"
              className="text-sm text-accent hover:underline"
            >
              Learn More →
            </a>
          </div>
        </div>
      </div>
    </>
  )
}
