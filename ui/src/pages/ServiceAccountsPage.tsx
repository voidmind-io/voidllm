import React, { useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Select } from '../components/ui/Select'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import {
  useServiceAccounts,
  useCreateServiceAccount,
  useDeleteServiceAccount,
  useUpdateServiceAccount,
} from '../hooks/useServiceAccounts'
import type {
  ServiceAccountResponse,
  CreateServiceAccountParams,
} from '../hooks/useServiceAccounts'
import { useTeams } from '../hooks/useTeams'
import { useToast } from '../hooks/useToast'
import { formatDate } from '../lib/utils'

// ---------------------------------------------------------------------------
// CreateServiceAccountDialog
// ---------------------------------------------------------------------------

interface CreateServiceAccountDialogProps {
  open: boolean
  onClose: () => void
  orgId: string
}

function CreateServiceAccountDialog({ open, onClose, orgId }: CreateServiceAccountDialogProps) {
  const [name, setName] = useState('')
  const [nameError, setNameError] = useState<string | undefined>()
  const [teamId, setTeamId] = useState('')
  const [teamError, setTeamError] = useState<string | undefined>()

  const { data: me } = useMe()
  const isOrgAdmin = me?.role === 'org_admin' || me?.is_system_admin === true

  const createServiceAccount = useCreateServiceAccount(orgId)
  const { data: teams } = useTeams(orgId)
  const { toast } = useToast()

  // For non-admins with exactly one team, auto-select it without an effect.
  const autoTeamId =
    !isOrgAdmin && teams?.data?.length === 1 ? teams.data[0].id : ''
  const effectiveTeamId = teamId || autoTeamId

  const teamOptions = isOrgAdmin
    ? [
        { value: '', label: 'Org-scoped (no team)' },
        ...(teams?.data?.map((t) => ({ value: t.id, label: t.name })) ?? []),
      ]
    : (teams?.data?.map((t) => ({ value: t.id, label: t.name })) ?? [])

  function handleClose() {
    setName('')
    setNameError(undefined)
    setTeamId('')
    setTeamError(undefined)
    onClose()
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()

    const trimmedName = name.trim()
    let hasError = false

    if (!trimmedName) {
      setNameError('Name is required')
      hasError = true
    } else {
      setNameError(undefined)
    }

    if (!isOrgAdmin && !effectiveTeamId) {
      setTeamError('Team is required')
      hasError = true
    } else {
      setTeamError(undefined)
    }

    if (hasError) return

    const params: CreateServiceAccountParams = {
      name: trimmedName,
      ...(effectiveTeamId ? { team_id: effectiveTeamId } : {}),
    }

    createServiceAccount.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Service account created' })
        handleClose()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to create service account',
        })
      },
    })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Create Service Account">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => setName(e.target.value)}
          placeholder="e.g. ci-deploy-bot"
          error={nameError}
          disabled={createServiceAccount.isPending}
        />
        <Select
          label="Team"
          options={teamOptions}
          value={effectiveTeamId}
          onChange={(val) => {
            setTeamId(val)
            if (val) setTeamError(undefined)
          }}
          placeholder={isOrgAdmin ? 'Org-scoped (no team)' : 'Select a team...'}
          error={teamError}
          disabled={createServiceAccount.isPending}
        />
        {!isOrgAdmin && teamOptions.length === 0 && (
          <p className="text-xs text-text-tertiary">
            You are not a member of any team. Contact your org admin to be added to a team first.
          </p>
        )}
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createServiceAccount.isPending}
          >
            Cancel
          </Button>
          <Button type="submit" loading={createServiceAccount.isPending}>
            Create Service Account
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// EditServiceAccountDialog
// ---------------------------------------------------------------------------

interface EditServiceAccountDialogProps {
  open: boolean
  onClose: () => void
  sa: ServiceAccountResponse
  orgId: string
}

function EditServiceAccountDialog({ open, onClose, sa, orgId }: EditServiceAccountDialogProps) {
  const [name, setName] = useState(sa.name)
  const [nameError, setNameError] = useState<string | undefined>()

  const updateServiceAccount = useUpdateServiceAccount(orgId)
  const { toast } = useToast()

  const isDirty = name.trim() !== sa.name

  function handleClose() {
    setName(sa.name)
    setNameError(undefined)
    onClose()
  }

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const trimmed = name.trim()
    if (!trimmed) {
      setNameError('Name is required')
      return
    }
    setNameError(undefined)

    updateServiceAccount.mutate(
      { saId: sa.id, name: trimmed },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Service account updated' })
          onClose()
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update service account',
          })
        },
      },
    )
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Edit Service Account">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Name"
          value={name}
          onChange={(e) => {
            setName(e.target.value)
            if (nameError) setNameError(undefined)
          }}
          error={nameError}
          disabled={updateServiceAccount.isPending}
        />

        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col gap-1">
            <span className="text-xs font-medium text-text-secondary uppercase tracking-wider">Scope</span>
            <div>
              {sa.team_id ? (
                <Badge variant="info">Team</Badge>
              ) : (
                <Badge variant="default">Org</Badge>
              )}
            </div>
          </div>
          <div className="flex flex-col gap-1">
            <span className="text-xs font-medium text-text-secondary uppercase tracking-wider">Keys</span>
            <span className="text-sm text-text-primary">{sa.key_count}</span>
          </div>
        </div>

        <div className="flex flex-col gap-1">
          <span className="text-xs font-medium text-text-secondary uppercase tracking-wider">Created</span>
          <span className="text-sm text-text-tertiary">{formatDate(sa.created_at)}</span>
        </div>

        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={updateServiceAccount.isPending}
          >
            Cancel
          </Button>
          <Button type="submit" loading={updateServiceAccount.isPending} disabled={!isDirty}>
            Save
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// ServiceAccountsPage
// ---------------------------------------------------------------------------

export default function ServiceAccountsPage() {
  const { data: me } = useMe()
  const orgId = me?.org_id ?? ''

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [deleteId, setDeleteId] = useState<string | null>(null)
  const [editSa, setEditSa] = useState<ServiceAccountResponse | null>(null)

  const { data: serviceAccounts, isLoading } = useServiceAccounts(orgId, cursor)
  const deleteServiceAccount = useDeleteServiceAccount(orgId)
  const { toast } = useToast()

  const columns: Column<ServiceAccountResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => <span className="text-text-primary">{row.name}</span>,
    },
    {
      key: 'scope',
      header: 'Scope',
      render: (row) =>
        row.team_id ? (
          <Badge variant="info">Team</Badge>
        ) : (
          <Badge variant="default">Org</Badge>
        ),
    },
    {
      key: 'key_count',
      header: 'Keys',
      render: (row) => (
        <span className="text-sm text-text-secondary">{row.key_count}</span>
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
        <div className="flex items-center justify-end gap-2">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setEditSa(row)}
            disabled={deleteServiceAccount.isPending}
          >
            Edit
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteId(row.id)}
            className="text-error hover:text-error"
            disabled={deleteServiceAccount.isPending}
          >
            Delete
          </Button>
        </div>
      ),
    },
  ]

  function handleDelete() {
    if (!deleteId) return
    deleteServiceAccount.mutate(deleteId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Service account deleted' })
        setDeleteId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete service account',
        })
        setDeleteId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="Service Accounts"
        description="Manage service accounts for automation"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create Service Account</Button>
        }
      />

      <Table<ServiceAccountResponse>
        columns={columns}
        data={serviceAccounts?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading && !!orgId}
        emptyMessage="No service accounts found"
        pagination={{
          cursor: cursor ?? null,
          hasMore: serviceAccounts?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (serviceAccounts?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(serviceAccounts.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <CreateServiceAccountDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
        orgId={orgId}
      />

      {editSa && (
        <EditServiceAccountDialog
          open={editSa !== null}
          onClose={() => setEditSa(null)}
          sa={editSa}
          orgId={orgId}
        />
      )}

      <ConfirmDialog
        open={deleteId !== null}
        onClose={() => setDeleteId(null)}
        onConfirm={handleDelete}
        title="Delete Service Account"
        description="Are you sure you want to delete this service account? Any keys associated with it will also be revoked."
        confirmLabel="Delete"
        loading={deleteServiceAccount.isPending}
      />
    </>
  )
}
