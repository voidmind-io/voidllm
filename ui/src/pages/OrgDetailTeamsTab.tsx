import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { StatCard } from '../components/ui/StatCard'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useTeams, useCreateTeam, useDeleteTeam } from '../hooks/useTeams'
import type { TeamResponse, CreateTeamParams } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'
import { deriveSlug } from '../lib/slug'

// ---------------------------------------------------------------------------
// CreateTeamDialog
// ---------------------------------------------------------------------------

interface CreateTeamDialogProps {
  open: boolean
  onClose: () => void
  orgId: string
}

function CreateTeamDialog({ open, onClose, orgId }: CreateTeamDialogProps) {
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [slugTouched, setSlugTouched] = useState(false)
  const [nameError, setNameError] = useState<string | undefined>()
  const [slugError, setSlugError] = useState<string | undefined>()

  const createTeam = useCreateTeam(orgId)
  const { toast } = useToast()

  function handleNameChange(e: React.ChangeEvent<HTMLInputElement>) {
    const value = e.target.value
    setName(value)
    if (!slugTouched) {
      setSlug(deriveSlug(value))
    }
  }

  function handleSlugChange(e: React.ChangeEvent<HTMLInputElement>) {
    setSlug(e.target.value)
    setSlugTouched(true)
  }

  function handleClose() {
    setName('')
    setSlug('')
    setSlugTouched(false)
    setNameError(undefined)
    setSlugError(undefined)
    onClose()
  }

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
    e.preventDefault()

    let valid = true

    const trimmedName = name.trim()
    if (!trimmedName) {
      setNameError('Name is required')
      valid = false
    } else {
      setNameError(undefined)
    }

    const trimmedSlug = slug.trim()
    if (!trimmedSlug) {
      setSlugError('Slug is required')
      valid = false
    } else {
      setSlugError(undefined)
    }

    if (!valid) return

    const params: CreateTeamParams = {
      name: trimmedName,
      slug: trimmedSlug,
    }

    createTeam.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Team created' })
        handleClose()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to create team',
        })
      },
    })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Create Team">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Name
          </p>
          <Input
            value={name}
            onChange={handleNameChange}
            placeholder="e.g. Backend Engineering"
            error={nameError}
            disabled={createTeam.isPending}
          />
        </div>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Slug
          </p>
          <Input
            value={slug}
            onChange={handleSlugChange}
            placeholder="e.g. backend-engineering"
            description="Lowercase letters, numbers, and hyphens only."
            error={slugError}
            disabled={createTeam.isPending}
            className="font-mono"
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createTeam.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createTeam.isPending}>
            Create Team
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// OrgDetailTeamsTab
// ---------------------------------------------------------------------------

export default function OrgDetailTeamsTab() {
  const { orgId = '' } = useParams<{ orgId: string }>()

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [deleteTeamId, setDeleteTeamId] = useState<string | null>(null)

  const { data: teams, isLoading } = useTeams(orgId, cursor)
  const deleteTeam = useDeleteTeam(orgId)
  const { toast } = useToast()

  const teamList = teams?.data ?? []
  const totalTeams = teamList.length
  const totalMembers = teamList.reduce((sum, t) => sum + (t.member_count ?? 0), 0)

  const columns: Column<TeamResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <span className="font-medium text-text-primary">{row.name}</span>
      ),
    },
    {
      key: 'slug',
      header: 'Slug',
      render: (row) => <Badge variant="muted">{row.slug}</Badge>,
    },
    {
      key: 'member_count',
      header: 'Members',
      render: (row) => (
        <span className="text-text-secondary">{row.member_count}</span>
      ),
    },
    {
      key: 'key_count',
      header: 'Keys',
      render: (row) => (
        <span className="text-text-secondary">{row.key_count}</span>
      ),
    },
    {
      key: 'created_at',
      header: 'Created',
      render: (row) => <TimeAgo date={row.created_at} />,
    },
    {
      key: 'actions',
      header: '',
      align: 'right',
      render: (row) => (
        <Button
          variant="ghost"
          size="sm"
          onClick={() => setDeleteTeamId(row.id)}
          className="!px-1.5 text-text-tertiary hover:text-error"
          disabled={deleteTeam.isPending}
          title="Delete team"
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
      ),
    },
  ]

  function handleDelete() {
    if (!deleteTeamId) return
    deleteTeam.mutate(deleteTeamId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Team deleted' })
        setDeleteTeamId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete team',
        })
        setDeleteTeamId(null)
      },
    })
  }

  return (
    <>
      {/* Create button */}
      <div className="flex justify-end mb-4">
        <Button onClick={() => setShowCreateDialog(true)}>Create Team</Button>
      </div>

      {/* Stat cards */}
      <div className="grid grid-cols-2 gap-4 mb-6">
        <StatCard
          label="Total Teams"
          value={totalTeams}
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
                d="M18 18.72a9.094 9.094 0 0 0 3.741-.479 3 3 0 0 0-4.682-2.72m.94 3.198.001.031c0 .225-.012.447-.037.666A11.944 11.944 0 0 1 12 21c-2.17 0-4.207-.576-5.963-1.584A6.062 6.062 0 0 1 6 18.719m12 0a5.971 5.971 0 0 0-.941-3.197m0 0A5.995 5.995 0 0 0 12 12.75a5.995 5.995 0 0 0-5.058 2.772m0 0a3 3 0 0 0-4.681 2.72 8.986 8.986 0 0 0 3.74.477m.94-3.197a5.971 5.971 0 0 0-.94 3.197M15 6.75a3 3 0 1 1-6 0 3 3 0 0 1 6 0Zm6 3a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Zm-13.5 0a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Z"
              />
            </svg>
          }
        />
        <StatCard
          label="Total Members"
          value={totalMembers}
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
                d="M15 19.128a9.38 9.38 0 0 0 2.625.372 9.337 9.337 0 0 0 4.121-.952 4.125 4.125 0 0 0-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 0 1 8.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0 1 11.964-3.07M12 6.375a3.375 3.375 0 1 1-6.75 0 3.375 3.375 0 0 1 6.75 0Zm8.25 2.25a2.625 2.625 0 1 1-5.25 0 2.625 2.625 0 0 1 5.25 0Z"
              />
            </svg>
          }
        />
      </div>

      <Table<TeamResponse>
        columns={columns}
        data={teamList}
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
                  d="M18 18.72a9.094 9.094 0 0 0 3.741-.479 3 3 0 0 0-4.682-2.72m.94 3.198.001.031c0 .225-.012.447-.037.666A11.944 11.944 0 0 1 12 21c-2.17 0-4.207-.576-5.963-1.584A6.062 6.062 0 0 1 6 18.719m12 0a5.971 5.971 0 0 0-.941-3.197m0 0A5.995 5.995 0 0 0 12 12.75a5.995 5.995 0 0 0-5.058 2.772m0 0a3 3 0 0 0-4.681 2.72 8.986 8.986 0 0 0 3.74.477m.94-3.197a5.971 5.971 0 0 0-.94 3.197M15 6.75a3 3 0 1 1-6 0 3 3 0 0 1 6 0Zm6 3a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Zm-13.5 0a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Z"
                />
              </svg>
            </div>
            <div className="text-center">
              <p className="text-sm font-medium text-text-primary">No teams yet</p>
              <p className="text-sm text-text-tertiary mt-1">Create a team to organize members</p>
            </div>
            <Button size="sm" onClick={() => setShowCreateDialog(true)}>
              Create Team
            </Button>
          </div>
        }
        pagination={{
          cursor: cursor ?? null,
          hasMore: teams?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (teams?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(teams.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <CreateTeamDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
        orgId={orgId}
      />

      <ConfirmDialog
        open={deleteTeamId !== null}
        onClose={() => setDeleteTeamId(null)}
        onConfirm={handleDelete}
        title="Delete Team"
        description="Are you sure you want to delete this team? All team keys and memberships will be permanently removed."
        confirmLabel="Delete"
        loading={deleteTeam.isPending}
      />
    </>
  )
}
