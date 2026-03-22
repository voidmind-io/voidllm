import React, { useState } from 'react'
import { useParams } from 'react-router-dom'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useTeams, useCreateTeam } from '../hooks/useTeams'
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

  function handleSubmit(e: React.FormEvent) {
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
        <Input
          label="Name"
          value={name}
          onChange={handleNameChange}
          placeholder="e.g. Backend Engineering"
          error={nameError}
          disabled={createTeam.isPending}
        />
        <Input
          label="Slug"
          value={slug}
          onChange={handleSlugChange}
          placeholder="e.g. backend-engineering"
          description="Lowercase letters, numbers, and hyphens only."
          error={slugError}
          disabled={createTeam.isPending}
          className="font-mono"
        />
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createTeam.isPending}
          >
            Cancel
          </Button>
          <Button type="submit" loading={createTeam.isPending}>
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

  const { data: teams, isLoading } = useTeams(orgId, cursor)

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
  ]

  return (
    <>
      <div className="flex justify-end mb-4">
        <Button onClick={() => setShowCreateDialog(true)}>Create Team</Button>
      </div>

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

      <CreateTeamDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
        orgId={orgId}
      />
    </>
  )
}
