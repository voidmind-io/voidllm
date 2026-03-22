import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Select } from '../components/ui/Select'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import { useUser } from '../hooks/useUsers'
import type { UserResponse } from '../hooks/useUsers'
import {
  useTeamMembers,
  useAddTeamMember,
  useRemoveTeamMember,
  useUpdateTeamMember,
} from '../hooks/useTeamMembers'
import type { TeamMembershipResponse } from '../hooks/useTeamMembers'
import type { OrgMembershipResponse } from '../hooks/useOrgMembers'
import { useToast } from '../hooks/useToast'
import apiClient from '../api/client'

// ---------------------------------------------------------------------------
// Role constants
// ---------------------------------------------------------------------------

const TEAM_ROLE_OPTIONS = [
  { value: 'member', label: 'Member' },
  { value: 'team_admin', label: 'Team Admin' },
]

function roleVariant(role: string): 'default' | 'muted' {
  return role === 'team_admin' || role === 'org_admin' || role === 'system_admin'
    ? 'default'
    : 'muted'
}

function roleLabel(role: string): string {
  const labels: Record<string, string> = {
    team_admin: 'Team Admin',
    org_admin: 'Org Admin',
    system_admin: 'System Admin',
    member: 'Member',
  }
  return labels[role] ?? role
}

// ---------------------------------------------------------------------------
// UserCell — fetches user data per row to enrich membership records
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
// AddMemberDialog
// ---------------------------------------------------------------------------

interface AddMemberDialogProps {
  open: boolean
  onClose: () => void
  orgId: string
  teamId: string
  existingMemberIds: Set<string>
}

interface MemberOption {
  userId: string
  label: string
  description: string
}

function useOrgMemberOptions(orgId: string, existingMemberIds: Set<string>) {
  return useQuery({
    queryKey: ['org-member-options', orgId],
    queryFn: async (): Promise<MemberOption[]> => {
      const membersRes = await apiClient<{ data: OrgMembershipResponse[] }>(
        `/orgs/${orgId}/members?limit=200`,
      )
      const users = await Promise.all(
        membersRes.data.map(async (m) => {
          try {
            const user = await apiClient<UserResponse>(`/users/${m.user_id}`)
            return {
              userId: m.user_id,
              label: user.display_name,
              description: user.email,
            }
          } catch {
            return { userId: m.user_id, label: m.user_id, description: '' }
          }
        }),
      )
      return users.filter((u) => !existingMemberIds.has(u.userId))
    },
    enabled: !!orgId,
    staleTime: 60_000,
  })
}

function AddMemberDialog({
  open,
  onClose,
  orgId,
  teamId,
  existingMemberIds,
}: AddMemberDialogProps) {
  const [userId, setUserId] = useState('')
  const [role, setRole] = useState('member')
  const [userIdError, setUserIdError] = useState<string | undefined>()

  const addMember = useAddTeamMember(orgId, teamId)
  const { toast } = useToast()
  const { data: memberOptions = [], isLoading: optionsLoading } =
    useOrgMemberOptions(orgId, existingMemberIds)

  const selectOptions = memberOptions.map((u) => ({
    value: u.userId,
    label: u.label,
    description: u.description,
  }))

  function handleClose() {
    setUserId('')
    setRole('member')
    setUserIdError(undefined)
    onClose()
  }

  function validate(): boolean {
    if (!userId) {
      setUserIdError('Please select a user')
      return false
    }
    setUserIdError(undefined)
    return true
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    if (!validate()) return

    addMember.mutate(
      { user_id: userId, role },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Member added to team' })
          handleClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to add member',
          })
        },
      },
    )
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Add Team Member">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Select
          label="User"
          options={selectOptions}
          value={userId}
          onChange={(v) => {
            setUserId(v)
            if (v) setUserIdError(undefined)
          }}
          searchable
          placeholder={optionsLoading ? 'Loading members…' : 'Search by name or email…'}
          error={userIdError}
          disabled={addMember.isPending || optionsLoading}
        />
        <Select
          label="Role"
          options={TEAM_ROLE_OPTIONS}
          value={role}
          onChange={setRole}
          disabled={addMember.isPending}
        />
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={handleClose} disabled={addMember.isPending}>
            Cancel
          </Button>
          <Button type="submit" loading={addMember.isPending}>
            Add Member
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// TeamMembersTab
// ---------------------------------------------------------------------------

export default function TeamMembersTab() {
  const { teamId = '' } = useParams<{ teamId: string }>()
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const canManage =
    me?.role === 'org_admin' ||
    me?.role === 'system_admin' ||
    me?.role === 'team_admin'

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showAddDialog, setShowAddDialog] = useState(false)
  const [removeMembershipId, setRemoveMembershipId] = useState<string | null>(null)
  const [pendingRoleChange, setPendingRoleChange] = useState<{
    membershipId: string
    newRole: string
  } | null>(null)

  const { data: members, isLoading } = useTeamMembers(orgId, teamId, cursor)
  const removeMember = useRemoveTeamMember(orgId, teamId)
  const updateMember = useUpdateTeamMember(orgId, teamId)
  const { toast } = useToast()

  const columns: Column<TeamMembershipResponse>[] = [
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
      render: (row) => {
        if (canManage && row.user_id !== me?.id) {
          return (
            <Select
              options={TEAM_ROLE_OPTIONS}
              value={row.role === 'team_admin' ? 'team_admin' : 'member'}
              onChange={(newRole) => {
                if (newRole === row.role) return
                setPendingRoleChange({ membershipId: row.id, newRole })
              }}
              disabled={updateMember.isPending}
            />
          )
        }
        return <Badge variant={roleVariant(row.role)}>{roleLabel(row.role)}</Badge>
      },
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
        if (!canManage) return null
        // Team admins can't remove themselves (would lose management access).
        // Org admins and system admins can remove themselves safely.
        const isSelf = row.user_id === me?.id
        const isHigherAdmin = me?.role === 'org_admin' || me?.role === 'system_admin'
        if (isSelf && !isHigherAdmin) return null
        return (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setRemoveMembershipId(row.id)}
            className="text-error hover:text-error"
            disabled={removeMember.isPending}
          >
            Remove
          </Button>
        )
      },
    },
  ]

  function handleRoleChange() {
    if (!pendingRoleChange) return
    const { membershipId, newRole } = pendingRoleChange
    updateMember.mutate(
      { membershipId, role: newRole },
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

  function handleRemove() {
    if (!removeMembershipId) return
    removeMember.mutate(removeMembershipId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Member removed from team' })
        setRemoveMembershipId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to remove member',
        })
        setRemoveMembershipId(null)
      },
    })
  }

  return (
    <>
      {canManage && (
        <div className="flex justify-end mb-4">
          <Button onClick={() => setShowAddDialog(true)}>Add Member</Button>
        </div>
      )}

      <Table<TeamMembershipResponse>
        columns={columns}
        data={members?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId && !!teamId}
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

      <AddMemberDialog
        open={showAddDialog}
        onClose={() => setShowAddDialog(false)}
        orgId={orgId}
        teamId={teamId}
        existingMemberIds={new Set(members?.data?.map((m) => m.user_id) ?? [])}
      />

      <ConfirmDialog
        open={removeMembershipId !== null}
        onClose={() => setRemoveMembershipId(null)}
        onConfirm={handleRemove}
        title="Remove Member"
        description="Are you sure you want to remove this member from the team?"
        confirmLabel="Remove"
        loading={removeMember.isPending}
      />

      <ConfirmDialog
        open={pendingRoleChange !== null}
        onClose={() => setPendingRoleChange(null)}
        onConfirm={handleRoleChange}
        title="Change Role"
        description={
          pendingRoleChange?.newRole === 'team_admin'
            ? 'Promote this user to Team Admin? They will gain administrative access to this team.'
            : 'Demote this user to Member? They will lose administrative access to this team.'
        }
        confirmLabel="Confirm"
        loading={updateMember.isPending}
      />
    </>
  )
}
