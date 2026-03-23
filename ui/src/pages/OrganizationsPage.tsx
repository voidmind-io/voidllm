import React, { useState } from 'react'
import { Link, Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { StatCard } from '../components/ui/StatCard'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import { useOrgs, useCreateOrg, useDeleteOrg } from '../hooks/useOrgs'
import type { OrgListItem, CreateOrgParams } from '../hooks/useOrgs'
import { useToast } from '../hooks/useToast'
import { deriveSlug } from '../lib/slug'

// ---------------------------------------------------------------------------
// CreateOrgDialog
// ---------------------------------------------------------------------------

interface CreateOrgDialogProps {
  open: boolean
  onClose: () => void
}

function CreateOrgDialog({ open, onClose }: CreateOrgDialogProps) {
  const [name, setName] = useState('')
  const [slug, setSlug] = useState('')
  const [slugTouched, setSlugTouched] = useState(false)
  const [nameError, setNameError] = useState<string | undefined>()
  const [slugError, setSlugError] = useState<string | undefined>()

  const createOrg = useCreateOrg()
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

    const params: CreateOrgParams = {
      name: trimmedName,
      slug: trimmedSlug,
    }

    createOrg.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Organization created' })
        handleClose()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to create organization',
        })
      },
    })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Create Organization">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Name
          </p>
          <Input
            value={name}
            onChange={handleNameChange}
            placeholder="e.g. Acme Corp"
            error={nameError}
            disabled={createOrg.isPending}
          />
        </div>
        <div>
          <p className="text-[10px] font-medium tracking-widest uppercase text-text-tertiary mb-1.5">
            Slug
          </p>
          <Input
            value={slug}
            onChange={handleSlugChange}
            placeholder="e.g. acme-corp"
            description="Used in URLs and API references. Lowercase letters, numbers, and hyphens only."
            error={slugError}
            disabled={createOrg.isPending}
            className="font-mono"
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createOrg.isPending}
          >
            Cancel
          </Button>
          <Button onClick={handleSubmit} loading={createOrg.isPending}>
            Create Organization
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// OrganizationsPage
// ---------------------------------------------------------------------------

export default function OrganizationsPage() {
  const { data: me } = useMe()

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [deleteOrgId, setDeleteOrgId] = useState<string | null>(null)

  const { data: orgs, isLoading } = useOrgs(cursor)
  const deleteOrg = useDeleteOrg()
  const { toast } = useToast()

  if (me && !me.is_system_admin) {
    return <Navigate to="/" replace />
  }

  const orgList = orgs?.data ?? []
  const totalOrgs = orgList.length
  const totalMembers = orgList.reduce((s, o) => s + (o.member_count ?? 0), 0)
  const totalTeams = orgList.reduce((s, o) => s + (o.team_count ?? 0), 0)

  const columns: Column<OrgListItem>[] = [
    {
      key: 'name',
      header: 'Name',
      render: (row) => (
        <Link
          to={`/orgs/${row.id}`}
          className="text-accent hover:underline no-underline font-medium"
        >
          {row.name}
        </Link>
      ),
    },
    {
      key: 'slug',
      header: 'Slug',
      render: (row) => <Badge variant="muted">{row.slug}</Badge>,
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
          onClick={() => setDeleteOrgId(row.id)}
          className="!px-1.5 text-text-tertiary hover:text-error"
          disabled={deleteOrg.isPending}
          title="Delete organization"
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
    if (!deleteOrgId) return
    deleteOrg.mutate(deleteOrgId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'Organization deleted' })
        setDeleteOrgId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete organization',
        })
        setDeleteOrgId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="Organizations"
        description="Manage all organizations in the system"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create Organization</Button>
        }
      />

      {/* Stat cards */}
      <div className="grid grid-cols-3 gap-4 mb-6">
        <StatCard label="Total Organizations" value={isLoading ? '—' : totalOrgs} iconColor="purple" icon={<svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M3.75 21h16.5M4.5 3h15M5.25 3v18m13.5-18v18M9 6.75h1.5m-1.5 3h1.5m-1.5 3h1.5m3-6H15m-1.5 3H15m-1.5 3H15M9 21v-3.375c0-.621.504-1.125 1.125-1.125h3.75c.621 0 1.125.504 1.125 1.125V21" /></svg>} />
        <StatCard label="Total Members" value={isLoading ? '—' : totalMembers} iconColor="blue" icon={<svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M15 19.128a9.38 9.38 0 0 0 2.625.372 9.337 9.337 0 0 0 4.121-.952 4.125 4.125 0 0 0-7.533-2.493M15 19.128v-.003c0-1.113-.285-2.16-.786-3.07M15 19.128v.106A12.318 12.318 0 0 1 8.624 21c-2.331 0-4.512-.645-6.374-1.766l-.001-.109a6.375 6.375 0 0 1 11.964-3.07M12 6.375a3.375 3.375 0 1 1-6.75 0 3.375 3.375 0 0 1 6.75 0Zm8.25 2.25a2.625 2.625 0 1 1-5.25 0 2.625 2.625 0 0 1 5.25 0Z" /></svg>} />
        <StatCard label="Total Teams" value={isLoading ? '—' : totalTeams} iconColor="green" icon={<svg className="w-4 h-4" fill="none" stroke="currentColor" strokeWidth={1.75} viewBox="0 0 24 24"><path strokeLinecap="round" strokeLinejoin="round" d="M18 18.72a9.094 9.094 0 0 0 3.741-.479 3 3 0 0 0-4.682-2.72m.94 3.198.001.031c0 .225-.012.447-.037.666A11.944 11.944 0 0 1 12 21c-2.17 0-4.207-.576-5.963-1.584A6.062 6.062 0 0 1 6 18.719m12 0a5.971 5.971 0 0 0-.941-3.197m0 0A5.995 5.995 0 0 0 12 12.75a5.995 5.995 0 0 0-5.058 2.772m0 0a3 3 0 0 0-4.681 2.72 8.986 8.986 0 0 0 3.74.477m.94-3.197a5.971 5.971 0 0 0-.94 3.197M15 6.75a3 3 0 1 1-6 0 3 3 0 0 1 6 0Zm6 3a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Zm-13.5 0a2.25 2.25 0 1 1-4.5 0 2.25 2.25 0 0 1 4.5 0Z" /></svg>} />
      </div>

      <Table<OrgListItem>
        columns={columns}
        data={orgList}
        keyExtractor={(row) => row.id}
        loading={isLoading}
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
                  d="M3.75 21h16.5M4.5 3h15M5.25 3v18m13.5-18v18M9 6.75h1.5m-1.5 3h1.5m-1.5 3h1.5m3-6H15m-1.5 3H15m-1.5 3H15M9 21v-3.375c0-.621.504-1.125 1.125-1.125h3.75c.621 0 1.125.504 1.125 1.125V21"
                />
              </svg>
            </div>
            <div className="text-center">
              <p className="text-sm font-medium text-text-primary">No organizations yet</p>
              <p className="text-sm text-text-tertiary mt-1">Create an organization to get started</p>
            </div>
            <Button size="sm" onClick={() => setShowCreateDialog(true)}>
              Create Organization
            </Button>
          </div>
        }
        pagination={{
          cursor: cursor ?? null,
          hasMore: orgs?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (orgs?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(orgs.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <CreateOrgDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
      />

      <ConfirmDialog
        open={deleteOrgId !== null}
        onClose={() => setDeleteOrgId(null)}
        onConfirm={handleDelete}
        title="Delete Organization"
        description="Are you sure you want to delete this organization? All teams, members, and keys will be permanently removed."
        confirmLabel="Delete"
        loading={deleteOrg.isPending}
      />
    </>
  )
}
