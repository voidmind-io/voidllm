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
import { StatCard } from '../components/ui/StatCard'
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
// Icons
// ---------------------------------------------------------------------------

function IconBot() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="11" width="18" height="10" rx="2" />
      <circle cx="12" cy="5" r="2" />
      <path d="M12 7v4" />
      <line x1="8" y1="16" x2="8" y2="16" strokeWidth="2.5" />
      <line x1="12" y1="16" x2="12" y2="16" strokeWidth="2.5" />
      <line x1="16" y1="16" x2="16" y2="16" strokeWidth="2.5" />
    </svg>
  )
}

function IconBuilding() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="2" y="3" width="20" height="18" rx="2" />
      <path d="M9 21V7" />
      <path d="M15 21V7" />
      <path d="M2 12h20" />
    </svg>
  )
}

function IconGroup() {
  return (
    <svg
      width="18"
      height="18"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  )
}

function IconPencil() {
  return (
    <svg
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <path d="M11 4H4a2 2 0 0 0-2 2v14a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-7" />
      <path d="M18.5 2.5a2.121 2.121 0 0 1 3 3L12 15l-4 1 1-4 9.5-9.5z" />
    </svg>
  )
}

function IconTrash() {
  return (
    <svg
      width="15"
      height="15"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.75"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14H6L5 6" />
      <path d="M10 11v6M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

function IconBotLarge() {
  return (
    <svg
      width="40"
      height="40"
      viewBox="0 0 24 24"
      fill="none"
      stroke="currentColor"
      strokeWidth="1.25"
      strokeLinecap="round"
      strokeLinejoin="round"
      aria-hidden="true"
    >
      <rect x="3" y="11" width="18" height="10" rx="2" />
      <circle cx="12" cy="5" r="2" />
      <path d="M12 7v4" />
      <line x1="8" y1="16" x2="8" y2="16" strokeWidth="2.5" />
      <line x1="12" y1="16" x2="12" y2="16" strokeWidth="2.5" />
      <line x1="16" y1="16" x2="16" y2="16" strokeWidth="2.5" />
    </svg>
  )
}

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

  async function handleSubmit(e: React.FormEvent | React.MouseEvent) {
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
      <form onSubmit={handleSubmit} className="space-y-5" noValidate>
        <div className="space-y-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
            Team
          </p>
          <Select
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
        </div>

        <div className="space-y-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
            Name
          </p>
          <Input
            value={name}
            onChange={(e) => setName(e.target.value)}
            placeholder="e.g. ci-deploy-bot"
            error={nameError}
            disabled={createServiceAccount.isPending}
          />
        </div>

        {!isOrgAdmin && teamOptions.length === 0 && (
          <p className="text-xs text-text-tertiary">
            You are not a member of any team. Contact your org admin to be added to a team first.
          </p>
        )}

        <div className="flex justify-end gap-2 pt-1">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createServiceAccount.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createServiceAccount.isPending}>
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

  function handleSubmit(e: React.FormEvent | React.MouseEvent) {
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
      <form onSubmit={handleSubmit} className="space-y-5" noValidate>
        <div className="space-y-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
            Name
          </p>
          <Input
            value={name}
            onChange={(e) => {
              setName(e.target.value)
              if (nameError) setNameError(undefined)
            }}
            error={nameError}
            disabled={updateServiceAccount.isPending}
          />
        </div>

        <div className="grid grid-cols-2 gap-4">
          <div className="flex flex-col gap-1.5">
            <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
              Scope
            </p>
            <div>
              {sa.team_id ? (
                <Badge variant="info">Team</Badge>
              ) : (
                <Badge variant="default">Org</Badge>
              )}
            </div>
          </div>
          <div className="flex flex-col gap-1.5">
            <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
              Keys
            </p>
            <span className="text-sm text-text-primary">{sa.key_count}</span>
          </div>
        </div>

        <div className="flex flex-col gap-1.5">
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary">
            Created
          </p>
          <span className="text-sm text-text-tertiary">{formatDate(sa.created_at)}</span>
        </div>

        <div className="flex justify-end gap-2 pt-1">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={updateServiceAccount.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={updateServiceAccount.isPending} disabled={!isDirty}>
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

  const allSAs = serviceAccounts?.data ?? []
  const orgScopedCount = allSAs.filter((sa) => !sa.team_id).length
  const teamScopedCount = allSAs.filter((sa) => !!sa.team_id).length

  const columns: Column<ServiceAccountResponse>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <span className="font-medium text-text-primary">{row.name}</span>
      ),
    },
    {
      key: 'scope',
      header: 'Scope',
      render: (row) =>
        row.team_id ? (
          <Badge variant="info">Team</Badge>
        ) : (
          <Badge variant="default">Organization</Badge>
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
        <div className="flex items-center justify-end gap-1">
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setEditSa(row)}
            disabled={deleteServiceAccount.isPending}
            className="!px-1.5"
            title="Edit"
          >
            <IconPencil />
          </Button>
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteId(row.id)}
            className="!px-1.5 text-error hover:text-error"
            disabled={deleteServiceAccount.isPending}
            title="Delete"
          >
            <IconTrash />
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

  const hasData = allSAs.length > 0
  const showEmptyState = !isLoading && !hasData && !!orgId

  return (
    <>
      <PageHeader
        title="Service Accounts"
        description="Manage service accounts for automation"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create Service Account</Button>
        }
      />

      <div className="grid grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Service Accounts"
          value={allSAs.length}
          icon={<IconBot />}
          iconColor="purple"
        />
        <StatCard
          label="Org-Scoped"
          value={orgScopedCount}
          icon={<IconBuilding />}
          iconColor="blue"
        />
        <StatCard
          label="Team-Scoped"
          value={teamScopedCount}
          icon={<IconGroup />}
          iconColor="green"
        />
      </div>

      {showEmptyState ? (
        <div className="flex flex-col items-center justify-center py-20 text-center">
          <span className="text-text-tertiary mb-4">
            <IconBotLarge />
          </span>
          <h3 className="text-base font-medium text-text-primary mb-1">
            No service accounts yet
          </h3>
          <p className="text-sm text-text-tertiary mb-6">
            Create a service account for CI/CD and automation
          </p>
          <Button onClick={() => setShowCreateDialog(true)}>
            Create Service Account
          </Button>
        </div>
      ) : (
        <Table<ServiceAccountResponse>
          columns={columns}
          data={allSAs}
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
      )}

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
