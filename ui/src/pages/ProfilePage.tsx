import React, { useState } from 'react'
import { PageHeader } from '../components/ui/PageHeader'
import { Input } from '../components/ui/Input'
import { Button } from '../components/ui/Button'
import { Badge } from '../components/ui/Badge'
import { useMe } from '../hooks/useMe'
import { useUpdateProfile } from '../hooks/useProfile'
import { useToast } from '../hooks/useToast'

function formatRole(role?: string): string {
  if (!role) return ''
  return role.split('_').map((w) => w.charAt(0).toUpperCase() + w.slice(1)).join(' ')
}

// ---------------------------------------------------------------------------
// EditProfileSection
// ---------------------------------------------------------------------------

interface EditProfileSectionProps {
  userId: string
  initialDisplayName: string
}

function EditProfileSection({ userId, initialDisplayName }: EditProfileSectionProps) {
  const [displayName, setDisplayName] = useState(initialDisplayName)
  const [displayNameError, setDisplayNameError] = useState<string | undefined>()
  const updateProfile = useUpdateProfile()
  const { toast } = useToast()

  const isDirty = displayName.trim() !== initialDisplayName

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    const trimmed = displayName.trim()
    if (!trimmed) {
      setDisplayNameError('Display name is required')
      return
    }
    setDisplayNameError(undefined)
    updateProfile.mutate(
      { userId, params: { display_name: trimmed } },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Profile updated' })
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to update profile',
          })
        },
      },
    )
  }

  return (
    <div className="rounded-lg border border-border bg-bg-secondary">
      <div className="px-6 py-4 border-b border-border">
        <h2 className="text-sm font-semibold text-text-primary">Edit Profile</h2>
      </div>
      <form onSubmit={handleSubmit} noValidate className="p-6 space-y-4">
        <Input
          label="Display Name"
          value={displayName}
          onChange={(e) => {
            setDisplayName(e.target.value)
            if (displayNameError) setDisplayNameError(undefined)
          }}
          placeholder="e.g. Jane Smith"
          error={displayNameError}
          disabled={updateProfile.isPending}
        />
        <div className="flex justify-end">
          <Button
            type="submit"
            loading={updateProfile.isPending}
            disabled={!isDirty}
          >
            Save
          </Button>
        </div>
      </form>
    </div>
  )
}

// ---------------------------------------------------------------------------
// ChangePasswordSection
// ---------------------------------------------------------------------------

interface ChangePasswordSectionProps {
  userId: string
}

function ChangePasswordSection({ userId }: ChangePasswordSectionProps) {
  const [currentPassword, setCurrentPassword] = useState('')
  const [newPassword, setNewPassword] = useState('')
  const [confirmPassword, setConfirmPassword] = useState('')
  const [currentPasswordError, setCurrentPasswordError] = useState<string | undefined>()
  const [newPasswordError, setNewPasswordError] = useState<string | undefined>()
  const [confirmPasswordError, setConfirmPasswordError] = useState<string | undefined>()

  const updateProfile = useUpdateProfile()
  const { toast } = useToast()

  function handleSubmit(e: React.FormEvent) {
    e.preventDefault()
    let hasError = false

    if (!currentPassword) {
      setCurrentPasswordError('Current password is required')
      hasError = true
    } else {
      setCurrentPasswordError(undefined)
    }

    if (!newPassword) {
      setNewPasswordError('New password is required')
      hasError = true
    } else if (newPassword.length < 8) {
      setNewPasswordError('Password must be at least 8 characters')
      hasError = true
    } else {
      setNewPasswordError(undefined)
    }

    if (!confirmPassword) {
      setConfirmPasswordError('Please confirm your new password')
      hasError = true
    } else if (newPassword !== confirmPassword) {
      setConfirmPasswordError('Passwords do not match')
      hasError = true
    } else {
      setConfirmPasswordError(undefined)
    }

    if (hasError) return

    updateProfile.mutate(
      {
        userId,
        params: {
          current_password: currentPassword,
          new_password: newPassword,
        },
      },
      {
        onSuccess: () => {
          toast({ variant: 'success', message: 'Password changed' })
          setCurrentPassword('')
          setNewPassword('')
          setConfirmPassword('')
        },
        onError: (err) => {
          toast({
            variant: 'error',
            message: err instanceof Error ? err.message : 'Failed to change password',
          })
        },
      },
    )
  }

  return (
    <div className="rounded-lg border border-border bg-bg-secondary">
      <div className="px-6 py-4 border-b border-border">
        <h2 className="text-sm font-semibold text-text-primary">Change Password</h2>
      </div>
      <form onSubmit={handleSubmit} noValidate className="p-6 space-y-4">
        <Input
          label="Current Password"
          type="password"
          value={currentPassword}
          onChange={(e) => {
            setCurrentPassword(e.target.value)
            if (currentPasswordError) setCurrentPasswordError(undefined)
          }}
          placeholder=""
          error={currentPasswordError}
          disabled={updateProfile.isPending}
          autoComplete="current-password"
        />
        <Input
          label="New Password"
          type="password"
          value={newPassword}
          onChange={(e) => {
            setNewPassword(e.target.value)
            if (newPasswordError) setNewPasswordError(undefined)
          }}
          placeholder=""
          error={newPasswordError}
          disabled={updateProfile.isPending}
          autoComplete="new-password"
          description="At least 8 characters"
        />
        <Input
          label="Confirm New Password"
          type="password"
          value={confirmPassword}
          onChange={(e) => {
            setConfirmPassword(e.target.value)
            if (confirmPasswordError) setConfirmPasswordError(undefined)
          }}
          placeholder=""
          error={confirmPasswordError}
          disabled={updateProfile.isPending}
          autoComplete="new-password"
        />
        <div className="flex justify-end">
          <Button type="submit" loading={updateProfile.isPending}>
            Change Password
          </Button>
        </div>
      </form>
    </div>
  )
}

// ---------------------------------------------------------------------------
// ProfilePage
// ---------------------------------------------------------------------------

export default function ProfilePage() {
  const { data: me, isLoading } = useMe()

  if (isLoading || !me) {
    return (
      <>
        <PageHeader title="Profile" description="Manage your account settings" />
        <div className="max-w-2xl space-y-6">
          <div className="rounded-lg border border-border bg-bg-secondary p-6 space-y-4">
            <div className="h-4 w-24 rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
            <div className="h-9 w-full rounded bg-bg-tertiary animate-pulse" />
          </div>
        </div>
      </>
    )
  }

  return (
    <>
      <PageHeader title="Profile" description="Manage your account settings" />

      <div className="max-w-2xl space-y-6">
        {/* Account Info */}
        <div className="rounded-lg border border-border bg-bg-secondary">
          <div className="px-6 py-4 border-b border-border">
            <h2 className="text-sm font-semibold text-text-primary">Account Info</h2>
          </div>
          <div className="p-6 space-y-4">
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-text-secondary uppercase tracking-wider">Email</span>
              <span className="text-sm text-text-primary">{me.email}</span>
            </div>
            <div className="flex flex-col gap-1">
              <span className="text-xs font-medium text-text-secondary uppercase tracking-wider">Role</span>
              <div>
                <Badge variant="default">{formatRole(me.role)}</Badge>
              </div>
            </div>
          </div>
        </div>

        <EditProfileSection userId={me.id} initialDisplayName={me.display_name} />
        <ChangePasswordSection userId={me.id} />
      </div>
    </>
  )
}
