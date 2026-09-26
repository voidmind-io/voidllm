import React, { useState } from 'react'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import TabSwitcher from '../components/ui/TabSwitcher'
import { StatCard } from '../components/ui/StatCard'
import { CopyButton } from '../components/ui/CopyButton'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import {
  useOrgMembers,
  useDeleteOrgMember,
  useUpdateOrgMember,
  type OrgMembershipResponse,
} from '../hooks/useOrgMembers'
import { useUser } from '../hooks/useUsers'
import { useCreateInvite } from '../hooks/useInvites'
import { useToast } from '../hooks/useToast'
import { parseLimitInput } from '../lib/utils'

// ---------------------------------------------------------------------------
// Role constants
// ---------------------------------------------------------------------------

const ROLE_OPTIONS = [
  { value: 'org_admin', label: 'Org Admin' },
  { value: 'member', label: 'Member' },
]

function roleVariant(role: string): 'default' | 'muted' {
  return role === 'org_admin' || role === 'system_admin' ? 'default' : 'muted'
}

function roleLabel(role: string): string {
  const labels: Record<string, string> = {
    org_admin: 'Org Admin',
    system_admin: 'System Admin',
    team_admin: 'Team Admin',
    member: 'Member',
  }
  return labels[role] ?? role
}

function isAdminRole(role: string): boolean {
  return role === 'org_admin' || role === 'system_admin'
}

// ---------------------------------------------------------------------------
// Limit helpers
// ---------------------------------------------------------------------------

function formatCompact(n: number): string {
  return new Intl.NumberFormat('en-US', { notation: 'compact', maximumFractionDigits: 1 }).format(n)
}

function formatLimitsSummary(m: OrgMembershipResponse): string {
  const parts: string[] = []
  if (m.requests_per_minute > 0) parts.push(`${formatCompact(m.requests_per_minute)} rpm`)
  if (m.requests_per_day > 0) parts.push(`${formatCompact(m.requests_per_day)} req/day`)
  if (m.daily_token_limit > 0) parts.push(`${formatCompact(m.daily_token_limit)} tok/day`)
  if (m.monthly_token_limit > 0) parts.push(`${formatCompact(m.monthly_token_limit)} tok/mo`)
  return parts.length > 0 ? parts.join(' · ') : 'Unlimited'
}

interface LimitsFormState {
  dailyTokenLimit: string
  monthlyTokenLimit: string
  requestsPerMinute: string
  requestsPerDay: string
}

function limitsFormFromMember(m: OrgMembershipResponse): LimitsFormState {
  return {
    dailyTokenLimit: m.daily_token_limit > 0 ? String(m.daily_token_limit) : '',
    monthlyTokenLimit: m.monthly_token_limit > 0 ? String(m.monthly_token_limit) : '',
    requestsPerMinute: m.requests_per_minute > 0 ? String(m.requests_per_minute) : '',
    requestsPerDay: m.requests_per_day > 0 ? String(m.requests_per_day) : '',
  }
}

// ---------------------------------------------------------------------------
// UserCell — fetches user data per row to enrich membership records
// ---------------------------------------------------------------------------

interface UserCellProps {
  userId: string
}

function UserCell({ userId }: UserCellProps) {
  const { data: user, isLoading } = useUser(userId)

  if (isLoading) {
    return (
      <div className="flex items-center gap-3">
        <div className="w-8 h-8 rounded-full bg-bg-tertiary animate-pulse shrink-0" />
        <div className="space-y-1.5">
          <div className="h-3.5 w-28 rounded bg-bg-tertiary animate-pulse" />
          <div className="h-3 w-36 rounded bg-bg-tertiary animate-pulse" />
        </div>
      </div>
    )
  }

  if (!user) {
    return (
      <div className="flex items-center gap-3">
        <div className="w-8 h-8 rounded-full bg-bg-tertiary shrink-0" />
        <span className="text-text-tertiary">—</span>
      </div>
    )
  }

  const initial = user.display_name?.charAt(0).toUpperCase() ?? '?'

  return (
    <div className="flex items-center gap-3">
      <div className="w-8 h-8 rounded-full bg-accent/15 text-accent font-semibold text-sm flex items-center justify-center shrink-0 select-none">
        {initial}
      </div>
      <div>
        <div className="text-sm font-medium text-text-primary leading-snug">
          {user.display_name}
        </div>
        <div className="text-xs text-text-tertiary leading-snug">{user.email}</div>
      </div>
    </div>
  )
}

// ---------------------------------------------------------------------------
// InviteUserDialog
// ---------------------------------------------------------------------------

interface InviteUserDialogProps {
  open: boolean
  onClose: () => void
  orgId: string
}

function InviteUserDialog({ open, onClose, orgId }: InviteUserDialogProps) {
  const [email, setEmail] = useState('')
  const [role, setRole] = useState('member')
  const [emailError, setEmailError] = useState<string | undefined>()
  const [inviteLink, setInviteLink] = useState<string | null>(null)

  const createInvite = useCreateInvite(orgId)
  const { toast } = useToast()

  function handleClose() {
    setEmail('')
    setRole('member')
    setEmailError(undefined)
    setInviteLink(null)
    onClose()
  }

  function validate(): boolean {
    const trimmed = email.trim()
    if (!trimmed) {
      setEmailError('Email is required')
      return false
    }
    if (!trimmed.includes('@')) {
      setEmailError('Enter a valid email address')
      return false
    }
    setEmailError(undefined)
    return true
  }

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
    e.preventDefault()
    if (!validate()) return

    createInvite.mutate(
      { email: email.trim(), role },
      {
        onSuccess: (data) => {
          if (data.token) {
            setInviteLink(`${window.location.origin}/invite/${data.token}`)
          } else {
            toast({ variant: 'success', message: 'Invite sent successfully' })
            handleClose()
          }
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to send invite',
          })
        },
      },
    )
  }

  // Link state — after successful invite creation
  if (inviteLink !== null) {
    return (
      <Dialog open={open} onClose={handleClose} title="Invite Sent">
        <div className="space-y-4">
          <p className="text-sm text-text-secondary">
            Share this invite link with{' '}
            <span className="text-text-primary font-medium">{email}</span>.
          </p>

          <div className="rounded-md bg-bg-tertiary border border-border px-3 py-2">
            <p className="text-xs text-text-tertiary break-all font-mono">{inviteLink}</p>
          </div>

          <div className="flex items-center gap-2">
            <CopyButton text={inviteLink} label="Copy Link" />
          </div>

          <div className="rounded-md bg-warning/10 border border-warning/20 px-3 py-2">
            <p className="text-xs text-warning">
              Share this link with the user. It expires in 7 days and can only be used once.
            </p>
          </div>

          <div className="flex justify-end pt-2">
            <Button onClick={handleClose}>Done</Button>
          </div>
        </div>
      </Dialog>
    )
  }

  // Form state
  return (
    <Dialog open={open} onClose={handleClose} title="Invite User">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Email
          </p>
          <Input
            type="email"
            value={email}
            onChange={(e) => setEmail(e.target.value)}
            placeholder="user@example.com"
            error={emailError}
            disabled={createInvite.isPending}
          />
        </div>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-2">
            Role
          </p>
          <TabSwitcher
            tabs={ROLE_OPTIONS.map(o => ({ key: o.value, label: o.label }))}
            activeKey={role}
            onChange={setRole}
            className="mb-0"
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={handleClose} disabled={createInvite.isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createInvite.isPending}>
            Send Invite
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// EditLimitsDialog
// ---------------------------------------------------------------------------

interface EditLimitsDialogProps {
  member: OrgMembershipResponse
  onClose: () => void
  orgId: string
}

function EditLimitsDialog({ member, onClose, orgId }: EditLimitsDialogProps) {
  const [form, setForm] = useState<LimitsFormState>(() => limitsFormFromMember(member))
  const updateMember = useUpdateOrgMember(orgId)
  const { toast } = useToast()

  function patch(field: keyof LimitsFormState) {
    return (e: React.ChangeEvent<HTMLInputElement>) =>
      setForm((prev) => ({ ...prev, [field]: e.target.value }))
  }

  const dailyTokenLimit = parseLimitInput(form.dailyTokenLimit)
  const monthlyTokenLimit = parseLimitInput(form.monthlyTokenLimit)
  const requestsPerMinute = parseLimitInput(form.requestsPerMinute)
  const requestsPerDay = parseLimitInput(form.requestsPerDay)

  const hasError = Boolean(
    dailyTokenLimit.error ||
      monthlyTokenLimit.error ||
      requestsPerMinute.error ||
      requestsPerDay.error,
  )

  const isDirty =
    dailyTokenLimit.value !== member.daily_token_limit ||
    monthlyTokenLimit.value !== member.monthly_token_limit ||
    requestsPerMinute.value !== member.requests_per_minute ||
    requestsPerDay.value !== member.requests_per_day

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
    e.preventDefault()
    if (hasError) return
    if (!isDirty) {
      onClose()
      return
    }

    const params: {
      daily_token_limit?: number
      monthly_token_limit?: number
      requests_per_minute?: number
      requests_per_day?: number
    } = {}

    if (dailyTokenLimit.value !== member.daily_token_limit) params.daily_token_limit = dailyTokenLimit.value
    if (monthlyTokenLimit.value !== member.monthly_token_limit) params.monthly_token_limit = monthlyTokenLimit.value
    if (requestsPerMinute.value !== member.requests_per_minute) params.requests_per_minute = requestsPerMinute.value
    if (requestsPerDay.value !== member.requests_per_day) params.requests_per_day = requestsPerDay.value

    updateMember.mutate(
      { membershipId: member.id, params },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Limits updated' })
          onClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update limits',
          })
        },
      },
    )
  }

  return (
    <Dialog open onClose={onClose} title="Edit User Limits">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <p className="text-xs text-text-secondary">
          These limits apply across all of this user&apos;s API keys in the organization. The most
          restrictive of org, team, user, and key limits always wins.
        </p>
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Daily Token Limit"
            type="number"
            min="0"
            value={form.dailyTokenLimit}
            onChange={patch('dailyTokenLimit')}
            placeholder="0 = unlimited"
            error={dailyTokenLimit.error}
            disabled={updateMember.isPending}
          />
          <Input
            label="Monthly Token Limit"
            type="number"
            min="0"
            value={form.monthlyTokenLimit}
            onChange={patch('monthlyTokenLimit')}
            placeholder="0 = unlimited"
            error={monthlyTokenLimit.error}
            disabled={updateMember.isPending}
          />
        </div>
        <div className="grid grid-cols-2 gap-4">
          <Input
            label="Requests per Minute"
            type="number"
            min="0"
            value={form.requestsPerMinute}
            onChange={patch('requestsPerMinute')}
            placeholder="0 = unlimited"
            error={requestsPerMinute.error}
            disabled={updateMember.isPending}
          />
          <Input
            label="Requests per Day"
            type="number"
            min="0"
            value={form.requestsPerDay}
            onChange={patch('requestsPerDay')}
            placeholder="0 = unlimited"
            error={requestsPerDay.error}
            disabled={updateMember.isPending}
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={onClose} disabled={updateMember.isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={updateMember.isPending} disabled={!isDirty || hasError}>
            Save Changes
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// OrgUsersPage
// ---------------------------------------------------------------------------

export default function OrgUsersPage() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showInviteDialog, setShowInviteDialog] = useState(false)
  const [deleteMembershipId, setDeleteMembershipId] = useState<string | null>(null)
  const [pendingRoleChange, setPendingRoleChange] = useState<{
    membershipId: string
    newRole: string
  } | null>(null)
  const [editLimitsMember, setEditLimitsMember] = useState<OrgMembershipResponse | null>(null)

  const { data: members, isLoading } = useOrgMembers(orgId, cursor)
  const deleteMember = useDeleteOrgMember(orgId)
  const updateMember = useUpdateOrgMember(orgId)
  const { toast } = useToast()

  const isAdmin = me?.role === 'org_admin' || me?.role === 'system_admin' || me?.is_system_admin === true

  if (me && !isAdmin) {
    return (
      <div className="rounded-lg border border-border bg-bg-secondary p-12 text-center">
        <p className="text-sm text-text-tertiary">Only organization admins can manage users.</p>
      </div>
    )
  }

  const memberList = members?.data ?? []
  const totalMembers = memberList.length
  const adminCount = memberList.filter((m) => isAdminRole(m.role)).length

  const columns: Column<OrgMembershipResponse>[] = [
    {
      key: 'user',
      header: 'User',
      render: (row) => <UserCell userId={row.user_id} />,
    },
    {
      key: 'role',
      header: 'Role',
      render: (row) => {
        if (isAdmin && row.user_id !== me?.id && row.role !== 'system_admin') {
          return (
            <Select
              options={ROLE_OPTIONS}
              value={row.role === 'org_admin' ? 'org_admin' : 'member'}
              onChange={(newRole) => {
                if (newRole === row.role) return
                setPendingRoleChange({
                  membershipId: row.id,
                  newRole,
                })
              }}
              disabled={updateMember.isPending}
            />
          )
        }
        return <Badge variant={roleVariant(row.role)}>{roleLabel(row.role)}</Badge>
      },
    },
    {
      key: 'limits',
      header: 'Limits',
      render: (row) => (
        <span className="text-xs text-text-tertiary">{formatLimitsSummary(row)}</span>
      ),
    },
    {
      key: 'created_at',
      header: 'Joined',
      render: (row) => <TimeAgo date={row.created_at} />,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      render: (row) => {
        const isSelf = row.user_id === me?.id
        return (
          <div className="flex items-center justify-end gap-0.5">
            {isAdmin && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setEditLimitsMember(row)}
                className="!px-1.5 text-text-tertiary hover:text-text-primary"
                disabled={updateMember.isPending}
                title="Edit limits"
              >
                <svg
                  className="w-4 h-4"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth={1.75}
                  viewBox="0 0 24 24"
                  aria-hidden="true"
                >
                  <path
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d="M16.862 4.487l1.687-1.688a1.875 1.875 0 112.652 2.652L6.832 19.82a4.5 4.5 0 01-1.897 1.13l-2.685.8.8-2.685a4.5 4.5 0 011.13-1.897L16.863 4.487zm0 0L19.5 7.125"
                  />
                </svg>
              </Button>
            )}
            {!isSelf && (
              <Button
                variant="ghost"
                size="sm"
                onClick={() => setDeleteMembershipId(row.id)}
                className="!px-1.5 text-text-tertiary hover:text-error"
                disabled={deleteMember.isPending}
                title="Remove user"
              >
                <svg
                  className="w-4 h-4"
                  fill="none"
                  stroke="currentColor"
                  strokeWidth={1.75}
                  viewBox="0 0 24 24"
                  aria-hidden="true"
                >
                  <path
                    strokeLinecap="round"
                    strokeLinejoin="round"
                    d="m14.74 9-.346 9m-4.788 0L9.26 9m9.968-3.21c.342.052.682.107 1.022.166m-1.022-.165L18.16 19.673a2.25 2.25 0 0 1-2.244 2.077H8.084a2.25 2.25 0 0 1-2.244-2.077L4.772 5.79m14.456 0a48.108 48.108 0 0 0-3.478-.397m-12 .562c.34-.059.68-.114 1.022-.165m0 0a48.11 48.11 0 0 1 3.478-.397m7.5 0v-.916c0-1.18-.91-2.164-2.09-2.201a51.964 51.964 0 0 0-3.32 0c-1.18.037-2.09 1.022-2.09 2.201v.916m7.5 0a48.667 48.667 0 0 0-7.5 0"
                  />
                </svg>
              </Button>
            )}
          </div>
        )
      },
    },
  ]

  function handleRoleChange() {
    if (!pendingRoleChange) return
    const { membershipId, newRole } = pendingRoleChange
    updateMember.mutate(
      { membershipId, params: { role: newRole } },
      {
        onSuccess: () => {
          setPendingRoleChange(null)
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update role',
          })
          setPendingRoleChange(null)
        },
      },
    )
  }

  function handleDelete() {
    if (!deleteMembershipId) return
    deleteMember.mutate(deleteMembershipId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'User removed from organization' })
        setDeleteMembershipId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message:
            err instanceof Error ? err.message : 'Failed to remove user',
        })
        setDeleteMembershipId(null)
      },
    })
  }

  return (
    <>
      {/* Invite button */}
      <div className="flex justify-end mb-4">
        <Button onClick={() => setShowInviteDialog(true)}>Invite User</Button>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Members"
          value={totalMembers}
          iconColor="purple"
          icon={
            <svg
              className="w-4 h-4"
              fill="none"
              stroke="currentColor"
              strokeWidth={1.75}
              viewBox="0 0 24 24"
              aria-hidden="true"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M15 19.128a9.38 9.38 0 0 0 2.625.372 9.337 9.337 0 0 0 4.121-.952 4.125 4.125 0 0 0-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 0 1 8.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0 1 11.964-3.07M12 6.375a3.375 3.375 0 1 1-6.75 0 3.375 3.375 0 0 1 6.75 0Zm8.25 2.25a2.625 2.625 0 1 1-5.25 0 2.625 2.625 0 0 1 5.25 0Z"
              />
            </svg>
          }
        />
        <StatCard
          label="Admins"
          value={adminCount}
          iconColor="blue"
          icon={
            <svg
              className="w-4 h-4"
              fill="none"
              stroke="currentColor"
              strokeWidth={1.75}
              viewBox="0 0 24 24"
              aria-hidden="true"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M9 12.75 11.25 15 15 9.75m-3-7.036A11.959 11.959 0 0 1 3.598 6 11.99 11.99 0 0 0 3 9.749c0 5.592 3.824 10.29 9 11.623 5.176-1.332 9-6.03 9-11.622 0-1.31-.21-2.571-.598-3.751h-.152c-3.196 0-6.1-1.248-8.25-3.285Z"
              />
            </svg>
          }
        />
        <StatCard
          label="Active"
          value={totalMembers}
          iconColor="green"
          icon={
            <svg
              className="w-4 h-4"
              fill="none"
              stroke="currentColor"
              strokeWidth={1.75}
              viewBox="0 0 24 24"
              aria-hidden="true"
            >
              <path
                strokeLinecap="round"
                strokeLinejoin="round"
                d="M9 12.75 11.25 15 15 9.75M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z"
              />
            </svg>
          }
        />
      </div>

      <Table<OrgMembershipResponse>
        columns={columns}
        data={memberList}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId}
        emptyState={
          <div className="flex flex-col items-center justify-center py-16 gap-4">
            <div className="w-12 h-12 rounded-full bg-bg-tertiary flex items-center justify-center">
              <svg
                className="w-6 h-6 text-text-tertiary"
                fill="none"
                stroke="currentColor"
                strokeWidth={1.5}
                viewBox="0 0 24 24"
                aria-hidden="true"
              >
                <path
                  strokeLinecap="round"
                  strokeLinejoin="round"
                  d="M15 19.128a9.38 9.38 0 0 0 2.625.372 9.337 9.337 0 0 0 4.121-.952 4.125 4.125 0 0 0-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 0 1 8.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0 1 11.964-3.07M12 6.375a3.375 3.375 0 1 1-6.75 0 3.375 3.375 0 0 1 6.75 0Zm8.25 2.25a2.625 2.625 0 1 1-5.25 0 2.625 2.625 0 0 1 5.25 0Z"
                />
              </svg>
            </div>
            <div className="text-center">
              <p className="text-sm font-medium text-text-primary">No members yet</p>
              <p className="text-sm text-text-tertiary mt-1">Invite users to join this organization</p>
            </div>
            <Button size="sm" onClick={() => setShowInviteDialog(true)}>
              Invite User
            </Button>
          </div>
        }
        pagination={{
          cursor: cursor ?? null,
          hasMore: members?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (members?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(members.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <InviteUserDialog
        open={showInviteDialog}
        onClose={() => setShowInviteDialog(false)}
        orgId={orgId}
      />

      {editLimitsMember !== null && (
        <EditLimitsDialog
          member={editLimitsMember}
          onClose={() => setEditLimitsMember(null)}
          orgId={orgId}
        />
      )}

      <ConfirmDialog
        open={deleteMembershipId !== null}
        onClose={() => setDeleteMembershipId(null)}
        onConfirm={handleDelete}
        title="Remove User"
        description="Are you sure you want to remove this user from the organization? Their API keys and team memberships will remain but they will lose org access."
        confirmLabel="Remove"
        loading={deleteMember.isPending}
      />

      <ConfirmDialog
        open={pendingRoleChange !== null}
        onClose={() => setPendingRoleChange(null)}
        onConfirm={handleRoleChange}
        title="Change Role"
        description={
          pendingRoleChange?.newRole === 'org_admin'
            ? 'Promote this user to Org Admin? They will gain full administrative access to this organization.'
            : 'Demote this user to Member? They will lose administrative access to this organization.'
        }
        confirmLabel="Confirm"
        loading={updateMember.isPending}
      />
    </>
  )
}
