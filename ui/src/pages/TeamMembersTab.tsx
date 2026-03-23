import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { useQuery } from '@tanstack/react-query'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Select } from '../components/ui/Select'
import { StatCard } from '../components/ui/StatCard'
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

function isAdminRole(role: string): boolean {
  return role === 'team_admin' || role === 'org_admin' || role === 'system_admin'
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

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
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
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            User
          </p>
          <Select
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
        </div>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Role
          </p>
          <Select
            options={TEAM_ROLE_OPTIONS}
            value={role}
            onChange={setRole}
            disabled={addMember.isPending}
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button variant="secondary" onClick={handleClose} disabled={addMember.isPending}>
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={addMember.isPending}>
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

  const memberList = members?.data ?? []
  const totalMembers = memberList.length
  const adminCount = memberList.filter((m) => isAdminRole(m.role)).length

  const columns: Column<TeamMembershipResponse>[] = [
    {
      key: 'user',
      header: 'User',
      render: (row) => <UserCell userId={row.user_id} />,
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
            className="!px-1.5 text-text-tertiary hover:text-error"
            disabled={removeMember.isPending}
            title="Remove member"
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
      {/* Header with Add button */}
      {canManage && (
        <div className="flex justify-end mb-4">
          <Button onClick={() => setShowAddDialog(true)}>Add Member</Button>
        </div>
      )}

      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-4 mb-6">
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
          label="Team Admins"
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
      </div>

      <Table<TeamMembershipResponse>
        columns={columns}
        data={memberList}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId && !!teamId}
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
              <p className="text-sm text-text-tertiary mt-1">Add members to start collaborating</p>
            </div>
            {canManage && (
              <Button size="sm" onClick={() => setShowAddDialog(true)}>
                Add Member
              </Button>
            )}
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

      <AddMemberDialog
        open={showAddDialog}
        onClose={() => setShowAddDialog(false)}
        orgId={orgId}
        teamId={teamId}
        existingMemberIds={new Set(memberList.map((m) => m.user_id))}
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
