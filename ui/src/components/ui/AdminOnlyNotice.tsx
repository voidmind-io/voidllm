interface AdminOnlyNoticeProps {
  message: string
}

/** Notice shown in place of a page the current role may not view. */
export function AdminOnlyNotice({ message }: AdminOnlyNoticeProps) {
  return (
    <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
      <p className="text-sm text-text-tertiary">{message}</p>
    </div>
  )
}
