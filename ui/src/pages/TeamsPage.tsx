import React, { useState } from 'react'
import { Link } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { TimeAgo } from '../components/ui/TimeAgo'
import { StatCard } from '../components/ui/StatCard'
import { useMe } from '../hooks/useMe'
import { useTeams, useCreateTeam, useDeleteTeam } from '../hooks/useTeams'
import type { TeamResponse, CreateTeamParams } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'
import { deriveSlug } from '../lib/slug'

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconTeam() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  )
}

function IconPerson() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">
      <path d="M20 21v-2a4 4 0 0 0-4-4H8a4 4 0 0 0-4 4v2" />
      <circle cx="12" cy="7" r="4" />
    </svg>
  )
}

function IconKey() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">
      <circle cx="7.5" cy="15.5" r="5.5" />
      <path d="M21 2l-9.6 9.6" />
      <path d="M15.5 7.5l3 3L22 7l-3-3" />
    </svg>
  )
}

function IconTrash() {
  return (
    <svg width="15" height="15" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round">
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

function IconTeamLarge() {
  return (
    <svg width="40" height="40" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.25" strokeLinecap="round" strokeLinejoin="round">
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  )
}

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

  async function handleSubmit(e: React.FormEvent | React.MouseEvent) {
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
      <form onSubmit={handleSubmit} className="space-y-5" noValidate>
        <div className="space-y-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
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

        <div className="space-y-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
            Slug
          </p>
          <Input
            value={slug}
            onChange={handleSlugChange}
            placeholder="e.g. backend-engineering"
            error={slugError}
            disabled={createTeam.isPending}
          />
          <p className="text-xs text-text-tertiary">
            Used in API calls and URLs. Auto-derived from name.
          </p>
        </div>

        <div className="flex justify-end gap-2 pt-1">
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
// TeamsPage
// ---------------------------------------------------------------------------

export default function TeamsPage() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''
  const isOrgAdmin = me?.role === 'org_admin' || me?.role === 'system_admin'

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [deleteTeamId, setDeleteTeamId] = useState<string | null>(null)

  const { data: teams, isLoading } = useTeams(orgId, cursor)
  const deleteTeam = useDeleteTeam(orgId)
  const { toast } = useToast()

  const totalMembers = (teams?.data ?? []).reduce((sum, t) => sum + t.member_count, 0)
  const totalKeys = (teams?.data ?? []).reduce((sum, t) => sum + t.key_count, 0)

  const columns: Column<TeamResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <Link
          to={`/teams/${row.id}`}
          className="text-accent hover:underline no-underline font-semibold"
        >
          {row.name}
        </Link>
      ),
    },
    {
      key: 'slug',
      header: 'Slug',
      render: (row) => (
        <Badge variant="muted">{row.slug}</Badge>
      ),
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
      render: (row) => {
        if (!isOrgAdmin) return null
        return (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteTeamId(row.id)}
            className="!px-1.5 text-error hover:text-error"
            title="Delete team"
            disabled={deleteTeam.isPending}
          >
            <IconTrash />
          </Button>
        )
      },
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

  const hasTeams = (teams?.data ?? []).length > 0
  const showEmptyState = !isLoading && !hasTeams && !!orgId

  return (
    <>
      <PageHeader
        title="Teams"
        description="Manage your teams"
        actions={
          isOrgAdmin ? (
            <Button onClick={() => setShowCreateDialog(true)}>Create Team</Button>
          ) : undefined
        }
      />

      <div className="grid grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Teams"
          value={teams?.data?.length ?? 0}
          icon={<IconTeam />}
          iconColor="purple"
        />
        <StatCard
          label="Total Members"
          value={totalMembers}
          icon={<IconPerson />}
          iconColor="blue"
        />
        <StatCard
          label="Total Keys"
          value={totalKeys}
          icon={<IconKey />}
          iconColor="green"
        />
      </div>

      {showEmptyState ? (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <span className="text-text-tertiary mb-4">
            <IconTeamLarge />
          </span>
          <h3 className="text-base font-medium text-text-primary mb-1">No teams yet</h3>
          <p className="text-sm text-text-tertiary mb-6">
            Create your first team to organize members and keys
          </p>
          {isOrgAdmin && (
            <Button onClick={() => setShowCreateDialog(true)}>
              Create Team
            </Button>
          )}
        </div>
      ) : (
        <Table<TeamResponse>
          columns={columns}
          data={teams?.data ?? []}
          keyExtractor={(row) => row.id}
          loading={isLoading && !!orgId}
          emptyMessage="No teams found"
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
      )}

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
        description="Are you sure you want to delete this team? All team memberships will be removed."
        confirmLabel="Delete"
        loading={deleteTeam.isPending}
      />
    </>
  )
}
