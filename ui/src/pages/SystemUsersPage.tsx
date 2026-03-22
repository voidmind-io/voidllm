import React, { useState } from 'react'
import { Navigate } from 'react-router-dom'
import { PageHeader } from '../components/ui/PageHeader'
import { Table } from '../components/ui/Table'
import type { Column } from '../components/ui/Table'
import { Dialog, ConfirmDialog } from '../components/ui/Dialog'
import { Badge } from '../components/ui/Badge'
import { Button } from '../components/ui/Button'
import { Input } from '../components/ui/Input'
import { Toggle } from '../components/ui/Toggle'
import { TimeAgo } from '../components/ui/TimeAgo'
import { useMe } from '../hooks/useMe'
import { useUsers, useCreateUser, useDeleteUser } from '../hooks/useUsers'
import type { UserResponse, CreateUserParams } from '../hooks/useUsers'
import { useToast } from '../hooks/useToast'

// ---------------------------------------------------------------------------
// CreateUserDialog
// ---------------------------------------------------------------------------

interface CreateUserDialogProps {
  open: boolean
  onClose: () => void
}

function CreateUserDialog({ open, onClose }: CreateUserDialogProps) {
  const [email, setEmail] = useState('')
  const [displayName, setDisplayName] = useState('')
  const [password, setPassword] = useState('')
  const [isSystemAdmin, setIsSystemAdmin] = useState(false)

  const [emailError, setEmailError] = useState<string | undefined>()
  const [displayNameError, setDisplayNameError] = useState<string | undefined>()
  const [passwordError, setPasswordError] = useState<string | undefined>()

  const createUser = useCreateUser()
  const { toast } = useToast()

  function handleClose() {
    setEmail('')
    setDisplayName('')
    setPassword('')
    setIsSystemAdmin(false)
    setEmailError(undefined)
    setDisplayNameError(undefined)
    setPasswordError(undefined)
    onClose()
  }

  async function handleSubmit(e: React.FormEvent) {
    e.preventDefault()

    let valid = true

    const trimmedEmail = email.trim()
    if (!trimmedEmail) {
      setEmailError('Email is required')
      valid = false
    } else if (!/^[^\s@]+@[^\s@]+\.[^\s@]+$/.test(trimmedEmail)) {
      setEmailError('Enter a valid email address')
      valid = false
    } else {
      setEmailError(undefined)
    }

    const trimmedName = displayName.trim()
    if (!trimmedName) {
      setDisplayNameError('Display name is required')
      valid = false
    } else {
      setDisplayNameError(undefined)
    }

    if (!password) {
      setPasswordError('Password is required')
      valid = false
    } else if (password.length < 8) {
      setPasswordError('Password must be at least 8 characters')
      valid = false
    } else {
      setPasswordError(undefined)
    }

    if (!valid) return

    const params: CreateUserParams = {
      email: trimmedEmail,
      display_name: trimmedName,
      password,
      is_system_admin: isSystemAdmin,
    }

    createUser.mutate(params, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'User created' })
        handleClose()
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to create user',
        })
      },
    })
  }

  return (
    <Dialog open={open} onClose={handleClose} title="Create User">
      <form onSubmit={handleSubmit} className="space-y-4" noValidate>
        <Input
          label="Email"
          type="email"
          value={email}
          onChange={(e) => setEmail(e.target.value)}
          placeholder="user@example.com"
          error={emailError}
          disabled={createUser.isPending}
        />
        <Input
          label="Display Name"
          value={displayName}
          onChange={(e) => setDisplayName(e.target.value)}
          placeholder="Jane Smith"
          error={displayNameError}
          disabled={createUser.isPending}
        />
        <Input
          label="Password"
          type="password"
          value={password}
          onChange={(e) => setPassword(e.target.value)}
          placeholder="Min. 8 characters"
          error={passwordError}
          disabled={createUser.isPending}
        />
        <div className="flex items-center gap-3">
          <Toggle
            checked={isSystemAdmin}
            onChange={setIsSystemAdmin}
            disabled={createUser.isPending}
            label="System Admin"
          />
        </div>
        <div className="flex justify-end gap-2 pt-2">
          <Button
            variant="secondary"
            onClick={handleClose}
            disabled={createUser.isPending}
          >
            Cancel
          </Button>
          <Button type="submit" loading={createUser.isPending}>
            Create User
          </Button>
        </div>
      </form>
    </Dialog>
  )
}

// ---------------------------------------------------------------------------
// SystemUsersPage
// ---------------------------------------------------------------------------

export default function SystemUsersPage() {
  const { data: me } = useMe()

  const [cursor, setCursor] = useState<string | undefined>()
  const [prevCursors, setPrevCursors] = useState<string[]>([])
  const [showCreateDialog, setShowCreateDialog] = useState(false)
  const [deleteUserId, setDeleteUserId] = useState<string | null>(null)

  const { data: users, isLoading } = useUsers(cursor)
  const deleteUser = useDeleteUser()
  const { toast } = useToast()

  if (me && !me.is_system_admin) {
    return <Navigate to="/" replace />
  }

  const columns: Column<UserResponse>[] = [
    {
      key: 'email',
      header: 'Email',
      render: (row) => (
        <span className="font-medium text-text-primary">{row.email}</span>
      ),
    },
    {
      key: 'display_name',
      header: 'Display Name',
      render: (row) => (
        <span className="text-text-secondary">{row.display_name}</span>
      ),
    },
    {
      key: 'is_system_admin',
      header: 'Admin',
      render: (row) =>
        row.is_system_admin ? <Badge variant="default">Admin</Badge> : null,
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
        if (row.id === me?.id) return null
        return (
          <Button
            variant="ghost"
            size="sm"
            onClick={() => setDeleteUserId(row.id)}
            className="text-error hover:text-error"
            disabled={deleteUser.isPending}
          >
            Delete
          </Button>
        )
      },
    },
  ]

  function handleDelete() {
    if (!deleteUserId) return
    deleteUser.mutate(deleteUserId, {
      onSuccess: () => {
        toast({ variant: 'success', message: 'User deleted' })
        setDeleteUserId(null)
      },
      onError: (err) => {
        toast({
          variant: 'error',
          message: err instanceof Error ? err.message : 'Failed to delete user',
        })
        setDeleteUserId(null)
      },
    })
  }

  return (
    <>
      <PageHeader
        title="Users"
        description="All system users"
        actions={
          <Button onClick={() => setShowCreateDialog(true)}>Create User</Button>
        }
      />

      <Table<UserResponse>
        columns={columns}
        data={users?.data ?? []}
        keyExtractor={(row) => row.id}
        loading={isLoading}
        emptyMessage="No users found"
        pagination={{
          cursor: cursor ?? null,
          hasMore: users?.has_more ?? false,
          hasPrevious: prevCursors.length > 0,
          onNext: () => {
            if (users?.next_cursor) {
              setPrevCursors((prev) => [...prev, cursor ?? ''])
              setCursor(users.next_cursor)
            }
          },
          onPrevious: () => {
            const prev = prevCursors[prevCursors.length - 1]
            setPrevCursors((p) => p.slice(0, -1))
            setCursor(prev || undefined)
          },
        }}
      />

      <CreateUserDialog
        open={showCreateDialog}
        onClose={() => setShowCreateDialog(false)}
      />

      <ConfirmDialog
        open={deleteUserId !== null}
        onClose={() => setDeleteUserId(null)}
        onConfirm={handleDelete}
        title="Delete User"
        description="Are you sure you want to delete this user? This action cannot be undone."
        confirmLabel="Delete"
        loading={deleteUser.isPending}
      />
    </>
  )
}
