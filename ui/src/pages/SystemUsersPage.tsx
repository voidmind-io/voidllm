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
import { StatCard } from '../components/ui/StatCard'
import { useMe } from '../hooks/useMe'
import { useUsers, useCreateUser, useDeleteUser } from '../hooks/useUsers'
import type { UserResponse, CreateUserParams } from '../hooks/useUsers'
import { useToast } from '../hooks/useToast'

// ---------------------------------------------------------------------------
// Icons
// ---------------------------------------------------------------------------

function IconUsers() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M17 21v-2a4 4 0 0 0-4-4H5a4 4 0 0 0-4 4v2" />
      <circle cx="9" cy="7" r="4" />
      <path d="M23 21v-2a4 4 0 0 0-3-3.87" />
      <path d="M16 3.13a4 4 0 0 1 0 7.75" />
    </svg>
  )
}

function IconShield() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M12 22s8-4 8-10V5l-8-3-8 3v7c0 6 8 10 8 10z" />
    </svg>
  )
}

function IconCloud() {
  return (
    <svg width="18" height="18" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.75" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <path d="M18 10h-1.26A8 8 0 1 0 9 20h9a5 5 0 0 0 0-10z" />
    </svg>
  )
}

function IconTrash() {
  return (
    <svg width="14" height="14" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="2" strokeLinecap="round" strokeLinejoin="round" aria-hidden="true">
      <polyline points="3 6 5 6 21 6" />
      <path d="M19 6l-1 14a2 2 0 0 1-2 2H8a2 2 0 0 1-2-2L5 6" />
      <path d="M10 11v6" />
      <path d="M14 11v6" />
      <path d="M9 6V4a1 1 0 0 1 1-1h4a1 1 0 0 1 1 1v2" />
    </svg>
  )
}

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

  async function handleSubmit(e: React.FormEvent | React.MouseEvent) {
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
          <Button onClick={handleSubmit} loading={createUser.isPending}>
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

  const allUsers = users?.data ?? []
  const adminCount = allUsers.filter((u) => u.is_system_admin).length
  const ssoCount = allUsers.filter((u) => u.auth_provider === 'oidc').length

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
      header: 'Role',
      render: (row) =>
        row.is_system_admin ? <Badge variant="default">Admin</Badge> : <Badge variant="muted">Member</Badge>,
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
          <button
            type="button"
            onClick={() => setDeleteUserId(row.id)}
            disabled={deleteUser.isPending}
            title="Delete user"
            className="p-1.5 rounded-md text-text-tertiary hover:text-error hover:bg-error/10 transition-colors disabled:opacity-40"
          >
            <IconTrash />
          </button>
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

      {/* Stat cards */}
      <div className="grid grid-cols-1 sm:grid-cols-3 gap-4 mb-6">
        <StatCard
          label="Total Users"
          value={isLoading ? '—' : allUsers.length}
          icon={<IconUsers />}
          iconColor="purple"
        />
        <StatCard
          label="System Admins"
          value={isLoading ? '—' : adminCount}
          icon={<IconShield />}
          iconColor="blue"
        />
        <StatCard
          label="SSO Users"
          value={isLoading ? '—' : ssoCount}
          icon={<IconCloud />}
          iconColor="green"
        />
      </div>

      <Table<UserResponse>
        columns={columns}
        data={allUsers}
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
