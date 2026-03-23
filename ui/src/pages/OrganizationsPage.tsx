import React, { useState } from 'react'
import { Link, Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
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
        <Input
          label="Name"
          value={name}
          onChange={handleNameChange}
          placeholder="e.g. Acme Corp"
          error={nameError}
          disabled={createOrg.isPending}
        />
        <Input
          label="Slug"
          value={slug}
          onChange={handleSlugChange}
          placeholder="e.g. acme-corp"
          description="Used in URLs and API references. Lowercase letters, numbers, and hyphens only."
          error={slugError}
          disabled={createOrg.isPending}
          className="font-mono"
        />
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
          className="text-error hover:text-error"
          disabled={deleteOrg.isPending}
        >
          Delete
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

      <Table<OrgListItem>
        columns={columns}
        data={orgs?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading}
        emptyMessage="No organizations found"
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
