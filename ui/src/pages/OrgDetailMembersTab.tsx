import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import { CopyButton } from '../components/ui/CopyButton'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import {
  useOrgMembers,
  useDeleteOrgMember,
  type OrgMembershipResponse,
} from '../hooks/useOrgMembers'
import { useUser } from '../hooks/useUsers'
import { useCreateInvite } from '../hooks/useInvites'
import { useToast } from '../hooks/useToast'

// ---------------------------------------------------------------------------
// Role helpers
// ---------------------------------------------------------------------------

const ROLE_OPTIONS = [
  { value: 'member', label: 'Member' },
  { value: 'org_admin', label: 'Org Admin' },
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

// ---------------------------------------------------------------------------
// UserCell
// ---------------------------------------------------------------------------

interface UserCellProps {
  userId: string
  field: 'email' | 'display_name'
}

function UserCell({ userId, field }: UserCellProps) {
  const { data: user, isLoading } = useUser(userId)

  if (isLoading) {
    return (
      <span className="inline-block h-3.5 w-32 rounded bg-bg-tertiary animate-pulse" />
    )
  }

  if (!user) {
    return <span className="text-text-tertiary">—</span>
  }

  return (
    <span className="text-text-primary">
      {field === 'email' ? user.email : user.display_name}
    </span>
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

  function handleSubmit(e: React.FormEvent) {
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

  return (
    <Dialog open={open} onClose={handleClose} title="Invite User">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="user@example.com"
          error={emailError}
          disabled={createInvite.isPending}
        />
        <Select
          label="Role"
          options={ROLE_OPTIONS}
          value={role}
          onChange={setRole}
          disabled={createInvite.isPending}
        />
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={handleClose} disabled={createInvite.isPending}>
            Cancel
          </Button>
          <Button type="submit" loading={createInvite.isPending}>
            Send Invite
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// OrgDetailMembersTab
// ---------------------------------------------------------------------------

export default function OrgDetailMembersTab() {
  const { orgId = '' } = useParams<{ orgId: string }>()
  const { data: me } = useMe()

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showInviteDialog, setShowInviteDialog] = useState(false)
  const [deleteMembershipId, setDeleteMembershipId] = useState<string | null>(null)

  const { data: members, isLoading } = useOrgMembers(orgId, cursor)
  const deleteMember = useDeleteOrgMember(orgId)
  const { toast } = useToast()

  const columns: Column<OrgMembershipResponse>[] = [
    {
      key: 'email',
      header: 'Email',
      render: (row) => <UserCell userId={row.user_id} field="email" />,
    },
    {
      key: 'display_name',
      header: 'Name',
      render: (row) => <UserCell userId={row.user_id} field="display_name" />,
    },
    {
      key: 'role',
      header: 'Role',
      render: (row) => (
        <Badge variant={roleVariant(row.role)}>{roleLabel(row.role)}</Badge>
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
        if (row.user_id === me?.id) return null
        return (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteMembershipId(row.id)}
            className="text-error hover:text-error"
            disabled={deleteMember.isPending}
          >
            Remove
          </Button>
        )
      },
    },
  ]

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
          message: err instanceof Error ? err.message : 'Failed to remove user',
        })
        setDeleteMembershipId(null)
      },
    })
  }

  return (
    <>
      <div className="flex justify-end mb-4">
        <Button onClick={() => setShowInviteDialog(true)}>Invite User</Button>
      </div>

      <Table<OrgMembershipResponse>
        columns={columns}
        data={members?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId}
        emptyMessage="No members found"
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

      <ConfirmDialog
        open={deleteMembershipId !== null}
        onClose={() => setDeleteMembershipId(null)}
        onConfirm={handleDelete}
        title="Remove User"
        description="Are you sure you want to remove this user from the organization? Their API keys and team memberships will remain but they will lose org access."
        confirmLabel="Remove"
        loading={deleteMember.isPending}
      />
    </>
  )
}
